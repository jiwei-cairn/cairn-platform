package api

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"

	"github.com/jiwei-cairn/cairn-pkg/trust"
)

// applyHubCert 把准入证书放进 X-Cairn-Cert。certJSON 为空则不改请求。
func applyHubCert(req *http.Request, certJSON string) {
	certJSON = strings.TrimSpace(certJSON)
	if certJSON == "" || req == nil {
		return
	}
	req.Header.Set(trust.HeaderCert, base64.StdEncoding.EncodeToString([]byte(certJSON)))
}

// readAccessCert 读取并解析导入的证书原文。
func readAccessCert(r *http.Request) ([]byte, *trust.AccessCert, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	cert, err := trust.ParseAccessCert(raw)
	if err != nil {
		return nil, nil, err
	}
	return raw, cert, nil
}
