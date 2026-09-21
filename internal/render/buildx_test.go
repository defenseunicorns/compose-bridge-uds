package render_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"defenseunicorns/uds-compose-bridge/internal/compose"
	"defenseunicorns/uds-compose-bridge/internal/render"
)

func TestGeneratedBuildComposePassesBuildxBakePrint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Buildx integration test in short mode")
	}

	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker CLI is not installed")
	}
	if output, err := exec.Command(dockerPath, "buildx", "version").CombinedOutput(); err != nil {
		t.Skipf("Docker Buildx is not available: %v: %s", err, output)
	}

	sourceDir := t.TempDir()
	composePath := filepath.Join(sourceDir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte(`name: bake-integration
services:
  server:
    build:
      context: .
      args:
        MESSAGE: hello
`), 0o644); err != nil {
		t.Fatalf("write compose fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile fixture: %v", err)
	}

	app, err := compose.LoadCanonicalFile(composePath)
	if err != nil {
		t.Fatalf("LoadCanonicalFile() error = %v", err)
	}
	outDir := filepath.Join(sourceDir, "out")
	if err := render.WritePackage(outDir, app); err != nil {
		t.Fatalf("WritePackage() error = %v", err)
	}

	archive := filepath.ToSlash(filepath.Join("image-archives", "server.tar"))
	command := exec.Command(dockerPath,
		"buildx", "bake",
		"--file", "build.compose.yaml",
		"--allow", "fs.read="+sourceDir,
		"--set", "server.platform+=linux/amd64",
		"--set", "server.platform+=linux/arm64",
		"--set", "server.output=type=oci,dest="+archive,
		"--print",
		"server",
	)
	command.Dir = outDir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker buildx bake --print rejected generated build definition: %v\n%s", err, output)
	}

	printed := string(output)
	for _, want := range []string{
		`"server"`,
		`"zarf.internal/bake-integration-server:dev"`,
		`"linux/amd64"`,
		`"linux/arm64"`,
		`"dest": "image-archives/server.tar"`,
		`"type": "oci"`,
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("expected Buildx Bake output to contain %q\n%s", want, printed)
		}
	}
}
