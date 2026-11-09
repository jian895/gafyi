package main

import (
	"regexp"
	"strings"
)

// WeChat risk control can silently drop bot messages that contain cloud-drive
// share links (Quark, Baidu, Aliyun…), and keep dropping later messages for a
// while: sendmessage still returns success. To avoid that, outbound text has
// such links "defanged" so WeChat no longer recognises them as links:
//   https://pan.quark.cn/s/abc  ->  pan[.]quark[.]cn/s/abc

var netdiskHosts = []string{
	"pan.quark.cn", "pan.baidu.com", "yun.baidu.com", "aliyundrive.com", "alipan.com",
	"115.com", "115cdn.com", "123pan.com", "123pan.cn", "123684.com", "123865.com", "123912.com",
	"lanzou.com", "lanzoui.com", "lanzoux.com", "lanzouw.com", "lanzouo.com",
	"cloud.189.cn", "share.weiyun.com", "pan.xunlei.com", "ctfile.com", "drive.uc.cn",
}

var hostRe = regexp.MustCompile(`(?i)(https?://)?((?:[a-z0-9-]+\.)+[a-z]{2,})(/[^\s，。、；）)」]*)?`)

func isNetdisk(host string) bool {
	host = strings.ToLower(host)
	for _, h := range netdiskHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func defangLinks(s string) string {
	return hostRe.ReplaceAllStringFunc(s, func(m string) string {
		p := hostRe.FindStringSubmatch(m)
		if !isNetdisk(p[2]) {
			return m
		}
		return strings.ReplaceAll(p[2], ".", "[.]") + p[3]
	})
}
