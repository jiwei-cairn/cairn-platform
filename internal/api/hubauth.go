package api

import (
	"net/http"
	"strings"

	"github.com/jiwei-cairn/cairn-platform/internal/config"
)

// hubCred 是平台访问 hub 升级和发布接口时使用的业务账户。零值表示不附带认证。
type hubCred struct {
	User string
	Pass string
}

func hubCredFrom(ota config.OTA) hubCred {
	return hubCred{User: strings.TrimSpace(ota.Username), Pass: ota.Password}
}

func (c hubCred) configured() bool {
	return strings.TrimSpace(c.User) != "" && c.Pass != ""
}

func (c hubCred) apply(req *http.Request) {
	if req == nil || !c.configured() {
		return
	}
	req.SetBasicAuth(strings.TrimSpace(c.User), c.Pass)
}
