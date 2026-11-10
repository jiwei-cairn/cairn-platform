// cairn-pack — .cairnpkg 打包工具（decisions.md P5）。
//
// 用法：
//   cairn-pack -name demo -version 1.0.0 -entrypoint bin/demo.bat \
//     -add build/demo.exe:bin/demo.exe:binary \
//     -add conf/app.conf:config/app.conf:config \
//     -out demo-1.0.0-windows-amd64.cairnpkg
//
// -add 格式：源文件:包内路径[:角色]，角色可省（entrypoint 自动 binary）。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jiwei-cairn/cairn-platform/internal/pack"
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
		parts := strings.Split(a, ":")
		if len(parts) < 2 {
			log.Fatalf("-add %q 格式应为 源:包内路径[:角色]", a)
		}
		role := ""
		if len(parts) >= 3 {
			role = parts[2]
		}
		files = append(files, pack.SourceFile{SrcPath: parts[0], PkgPath: parts[1], Role: role})
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
