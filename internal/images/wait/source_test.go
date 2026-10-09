package wait

import (
	"context"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSourceCrossCompilesStandalone(t *testing.T) {
	for _, target := range []struct {
		arch    string
		machine elf.Machine
	}{
		{arch: "amd64", machine: elf.EM_X86_64},
		{arch: "arm64", machine: elf.EM_AARCH64},
	} {
		t.Run("linux/"+target.arch, func(t *testing.T) {
			dir := t.TempDir()
			mainFile, err := filepath.Abs(filepath.Join("cmd", "main.go"))
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "wait")
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, mainFile)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GOOS=linux", "GOARCH="+target.arch,
				"CGO_ENABLED=0", "GOPROXY=off", "GO111MODULE=off",
				"GOWORK=off", "GOTOOLCHAIN=local",
			)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("standalone cross-compilation: %v\n%s", err, output)
			}
			file, err := elf.Open(binary)
			if err != nil {
				t.Fatalf("open ELF binary: %v", err)
			}
			defer file.Close()
			if file.Machine != target.machine {
				t.Errorf("ELF architecture = %s, want %s", file.Machine, target.machine)
			}
			for _, program := range file.Progs {
				if program.Type == elf.PT_INTERP {
					t.Error("ELF binary contains a PT_INTERP dynamic interpreter")
				}
			}
		})
	}
}

func TestSourceBuildsStandalone(t *testing.T) {
	dir := t.TempDir()
	mainFile, err := filepath.Abs(filepath.Join("cmd", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "wait"), mainFile)
	cmd.Env = append(os.Environ(), "GO111MODULE=off", "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("standalone build: %v\n%s", err, output)
	}
}
