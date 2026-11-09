package main

import "testing"

func TestDefangLinks(t *testing.T) {
	cases := map[string]string{
		"夸克 https://pan.quark.cn/s/abc123 提取码 x":                      "夸克 pan[.]quark[.]cn/s/abc123 提取码 x",
		"百度 pan.baidu.com/s/1Xy?pwd=ab，阿里 https://www.alipan.com/s/q": "百度 pan[.]baidu[.]com/s/1Xy?pwd=ab，阿里 www[.]alipan[.]com/s/q",
		"官网 https://grok.com/x 不改":                                    "官网 https://grok.com/x 不改",
		"邮箱 a@baidu.com 不改":                                           "邮箱 a@baidu.com 不改",
	}
	for in, want := range cases {
		if got := defangLinks(in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}
