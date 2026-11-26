package pack

import (
	"regexp"
	"strconv"
	"strings"
)

var semverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// IsSemVer 是否为 MAJOR.MINOR.PATCH[-pre][+build]。
func IsSemVer(v string) bool {
	return semverPattern.MatchString(strings.TrimSpace(v))
}

// MustCompareSemVer 比较两个语义化版本。返回值 >0 表示 a 更高。
// 构建元数据（+build）不参与比较。预发布版低于同号正式版。
// 无法解析时视为相等，避免坏版本号把进程打崩。
func MustCompareSemVer(a, b string) int {
	ac, ap := splitSemver(a)
	bc, bp := splitSemver(b)
	if c := compareCore(ac, bc); c != 0 {
		return c
	}
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	default:
		return comparePre(ap, bp)
	}
}

func splitSemver(v string) (core, pre string) {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

func compareCore(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		av, bv := 0, 0
		if i < len(ap) {
			av, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			bv, _ = strconv.Atoi(bp[i])
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}

func comparePre(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		if i >= len(ap) {
			return -1
		}
		if i >= len(bp) {
			return 1
		}
		ai, aNum := strconv.Atoi(ap[i])
		bi, bNum := strconv.Atoi(bp[i])
		switch {
		case aNum == nil && bNum == nil:
			if ai != bi {
				if ai > bi {
					return 1
				}
				return -1
			}
		case aNum == nil:
			return -1
		case bNum == nil:
			return 1
		default:
			if ap[i] != bp[i] {
				if ap[i] > bp[i] {
					return 1
				}
				return -1
			}
		}
	}
	return 0
}
