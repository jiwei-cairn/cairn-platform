// cairn-pack — .cairnpkg 打包工具（decisions.md P5）。
//
// 用法：
//
//	cairn-pack -name demo -version 1.0.0 -entrypoint bin/demo.bat \
//	  -add build/demo.exe:bin/demo.exe:binary \
//	  -add conf/app.conf:config/app.conf:config \
//	  -out demo-1.0.0-windows-amd64.cairnpkg
//
// -add 格式：源文件:包内路径[:角色]，角色可省（entrypoint 自动 binary）。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jiwei-cairn/cairn-pkg/pack"
	goruntime "runtime"
)

type addFlag []string

func (a *addFlag) String() string     { return strings.Join(*a, ", ") }
func (a *addFlag) Set(v string) error { *a = append(*a, v); return nil }

func main() {
	var (
		name    = flag.String("name", "", "服务名（必需）")
		version = flag.String("version", "", "版本（必需，语义化）")
		osName  = flag.String("os", goruntime.GOOS, "目标平台 linux|windows")
		arch    = flag.String("arch", goruntime.GOARCH, "目标架构 amd64|arm64|arm")
		entry   = flag.String("entrypoint", "", "主程序包内路径（必需）")
		display = flag.String("display", "", "展示名（可选）")
		notes   = flag.String("notes", "", "发布说明（可选）")
		out     = flag.String("out", "", "输出路径（缺省 {name}-{version}-{os}-{arch}.cairnpkg）")
		shaOnly = flag.String("sha256-of", "", "仅计算指定文件的 sha256 后退出（发布登记用）")
		adds    addFlag
	)
	flag.Var(&adds, "add", "源文件:包内路径[:角色]，可重复")
	flag.Parse()

	if *shaOnly != "" {
		sum, err := pack.Sha256File(*shaOnly)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(sum)
		return
	}

	if *name == "" || *version == "" || *entry == "" || len(adds) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	var files []pack.SourceFile
	for _, a := range adds {
		src, pkg, role, err := parseAdd(a)
		if err != nil {
			log.Fatalf("-add %q: %v（格式 源:包内路径[:角色]）", a, err)
		}
		files = append(files, pack.SourceFile{SrcPath: src, PkgPath: pkg, Role: role})
	}
	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("%s-%s-%s-%s.cairnpkg", *name, *version, *osName, *arch)
	}
	sum, err := pack.Build(pack.BuildOptions{
		Name: *name, Version: *version, Os: *osName, Arch: *arch,
		Entrypoint: *entry, DisplayName: *display, ReleaseNotes: *notes,
	}, files, outPath)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("打包成功: %s\n  整包 sha256: %s\n", outPath, sum)
}

// parseAdd 解析 -add 参数：源文件:包内路径[:角色]。
//
// 必须从右往左切分：Windows 源路径自带盘符冒号（E:\build\svc.exe），
// 从左切会把 "E" 当成源文件、"\build\svc.exe" 当成包内路径——
// 表现为「包内路径必须以 / 分隔符」这类莫名其妙的报错（实测踩坑）。
//
// 规则：
//   - 先看最后一段是否为已知角色（binary/dependency/config/data）；
//     是则 role 取它并剥离，否则最后一段属于包内路径。
//   - 再从右侧第一个冒号处切出包内路径，左侧整段即源文件路径。
func parseAdd(a string) (src, pkgPath, role string, err error) {
	s := strings.TrimSpace(a)
	if s == "" {
		return "", "", "", fmt.Errorf("参数为空")
	}
	// 角色只在第三段且为已知值时成立（避免把 "C:\x:bin/a" 的 bin/a 误判）。
	if i := strings.LastIndex(s, ":"); i >= 0 {
		if cand := s[i+1:]; isKnownRole(cand) {
			role = cand
			s = s[:i]
		}
	}
	i := strings.LastIndex(s, ":")
	// 盘符冒号不是分隔符：单字母且位于首位（形如 "E:\..."）视为盘符。
	if i == 1 && len(s) > 1 && isDriveLetter(s[0]) && (s[2] == '\\' || s[2] == '/') {
		return "", "", "", fmt.Errorf("缺少包内路径（源路径的盘符冒号不能作为分隔符）")
	}
	if i <= 0 || i == len(s)-1 {
		return "", "", "", fmt.Errorf("缺少包内路径分隔冒号")
	}
	src = s[:i]
	pkgPath = s[i+1:]
	// 包内路径统一正斜杠（Windows 上用户常写反斜杠）。
	pkgPath = strings.ReplaceAll(pkgPath, `\`, "/")
	if src == "" || pkgPath == "" {
		return "", "", "", fmt.Errorf("源文件或包内路径为空")
	}
	return src, pkgPath, role, nil
}

// isKnownRole 判断是否为包格式定义的文件角色。
func isKnownRole(s string) bool {
	switch s {
	case pack.RoleBinary, pack.RoleDependency, pack.RoleConfig, pack.RoleData:
		return true
	}
	return false
}

// isDriveLetter 判断是否为 Windows 盘符。
func isDriveLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
