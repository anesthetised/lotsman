package main

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	meminfo454 = "MemTotal:         465000 kB\nMemFree:           12345 kB\n"
	mib        = 1 << 20
)

func TestMemoryLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       string
		meminfo   string
		memoryMax string
		want      int64 // 0 means no limit
	}{
		{"unset env uses MemTotal", "", meminfo454, "", 465000 * 1024 / 100 * 40},
		{"explicit env wins", "200MiB", meminfo454, "", 0},
		{"cgroup cap below RAM", "", meminfo454, "209715200\n", 200 * mib / 100 * 40},
		{"cgroup cap above RAM", "", meminfo454, "99999999999\n", 465000 * 1024 / 100 * 40},
		{"cgroup max is unlimited", "", meminfo454, "max\n", 465000 * 1024 / 100 * 40},
		{"cgroup garbage ignored", "", meminfo454, "lots", 465000 * 1024 / 100 * 40},
		{"cgroup zero ignored", "", meminfo454, "0\n", 465000 * 1024 / 100 * 40},
		{"no meminfo", "", "", "", 0},
		{"no MemTotal line", "", "MemFree: 1 kB\n", "", 0},
		{"MemTotal garbage", "", "MemTotal: lots kB\n", "", 0},
		{"MemTotal without unit", "", "MemTotal: 465000\n", "", 0},
		{"MemTotal zero", "", "MemTotal: 0 kB\n", "", 0},
		{"MemTotal negative", "", "MemTotal: -5 kB\n", "", 0},
		{"MemTotal overflows", "", "MemTotal: 99999999999999999 kB\n", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := memoryLimit(tc.env, tc.meminfo, tc.memoryMax)
			if got != tc.want || ok != (tc.want != 0) {
				t.Fatalf("memoryLimit = %d, %v; want %d", got, ok, tc.want)
			}
		})
	}
}

func TestCgroupMaxPath(t *testing.T) {
	for _, tc := range []struct {
		name, self, want string
	}{
		{"service", "0::/system.slice/lotsman.service\n", "/cg/system.slice/lotsman.service/memory.max"},
		{"container root", "0::/\n", "/cg/memory.max"},
		{"hybrid v1 and v2", "12:memory:/x\n0::/y\n", "/cg/y/memory.max"},
		{"v1 only", "12:memory:/x\n", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cgroupMaxPath("/cg", tc.self)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("cgroupMaxPath = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
}

func TestReadMemoryLimit(t *testing.T) {
	write := func(root, name, content string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proc, cg := t.TempDir(), t.TempDir()

	if _, ok := readMemoryLimit("", proc, cg); ok {
		t.Fatal("limit set without /proc/meminfo")
	}

	write(proc, "meminfo", meminfo454)
	want := int64(465000 * 1024 / 100 * 40)
	if got, ok := readMemoryLimit("", proc, cg); !ok || got != want {
		t.Fatalf("no cgroup files: %d, %v; want %d", got, ok, want)
	}
	if _, ok := readMemoryLimit("1GiB", proc, cg); ok {
		t.Fatal("limit set although GOMEMLIMIT is")
	}

	write(proc, "self/cgroup", "0::/system.slice/lotsman.service\n")
	if got, ok := readMemoryLimit("", proc, cg); !ok || got != want {
		t.Fatalf("cgroup without memory.max: %d, %v; want %d", got, ok, want)
	}
	write(cg, "system.slice/lotsman.service/memory.max", "104857600\n")
	if got, ok := readMemoryLimit("", proc, cg); !ok || got != 100*mib/100*40 {
		t.Fatalf("capped: %d, %v; want %d", got, ok, 100*mib/100*40)
	}
}
