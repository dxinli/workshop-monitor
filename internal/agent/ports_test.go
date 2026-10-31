package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// 端点表是部署事实，不写进点表。给的键缺一项必须报错而不是回落到 /dev/ttyV*——
// 回落的表现是「总线全超时」，现场与真故障分不开。
func TestPortMapResolve(t *testing.T) {
	cases := []struct {
		name    string
		ports   PortMap
		want    string
		wantErr bool
	}{
		{name: "未给端点表时用点表声明值", ports: nil, want: "/dev/ttyV0"},
		{name: "命中端点表", ports: PortMap{"ESN-ZX-0001.A": "/tmp/a-agent"}, want: "/tmp/a-agent"},
		{name: "缺该总线", ports: PortMap{"ESN-ZX-0001.B": "/tmp/b-agent"}, wantErr: true},
		{name: "空值等同缺失", ports: PortMap{"ESN-ZX-0001.A": ""}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.ports.Resolve("ESN-ZX-0001", "A", "/dev/ttyV0")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应报错，实得 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("端口 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestLoadPorts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ports.json")
	body := `{"ESN-ZX-0001.A": "/tmp/a", "ESN-ZX-0001.B": "/tmp/b"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ports, err := LoadPorts(path)
	if err != nil {
		t.Fatal(err)
	}
	if ports["ESN-ZX-0001.A"] != "/tmp/a" || ports["ESN-ZX-0001.B"] != "/tmp/b" {
		t.Fatalf("解析结果 = %v", ports)
	}
}

func TestLoadPortsMissingFile(t *testing.T) {
	if _, err := LoadPorts(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("文件不存在时必须报错，不能静默返回空表")
	}
}
