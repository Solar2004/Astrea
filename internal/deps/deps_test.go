package deps

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLookup(t *testing.T) {
	if d, err := Lookup("GIT"); err != nil || d.Name != "git" {
		t.Fatalf("Lookup(GIT) = %+v, %v", d, err)
	}
	d, err := Lookup("winget:Mozilla.Firefox")
	if err != nil || d.Winget != "Mozilla.Firefox" || !d.raw() {
		t.Fatalf("raw winget spec = %+v, %v", d, err)
	}
	if _, err := Lookup("brew:wget"); err == nil {
		t.Fatal("unknown manager accepted")
	}
	if _, err := Lookup("nope"); err == nil {
		t.Fatal("unknown dependency accepted")
	}
}

func TestCheckFiles(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.dll")
	b := filepath.Join(dir, "b.dll")
	if err := os.WriteFile(a, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASTREA_TEST_DIR", dir)
	ctx := context.Background()

	// Library: every file is required.
	lib := Dep{Name: "lib", Files: []string{"%ASTREA_TEST_DIR%/a.dll", "%ASTREA_TEST_DIR%/b.dll"}}
	if st := Check(ctx, lib); st.Present {
		t.Fatalf("library with a missing file reported present: %+v", st)
	}
	if err := os.WriteFile(b, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if st := Check(ctx, lib); !st.Present || st.Path != a {
		t.Fatalf("complete library not detected: %+v", st)
	}

	// Binary with fallback locations: any one will do.
	bin := Dep{Name: "bin", Bins: []string{"astrea-surely-missing"}, Files: []string{"$ASTREA_TEST_DIR/none", "$ASTREA_TEST_DIR/b.dll"}}
	if st := Check(ctx, bin); !st.Present || st.Path != b {
		t.Fatalf("fallback file not detected: %+v", st)
	}
}

func TestCheckOnPath(t *testing.T) {
	name := "go"
	if _, err := lookPath(name); err != nil {
		t.Skip("go not on PATH")
	}
	st := Check(context.Background(), Dep{Name: "go", Bins: []string{"astrea-missing", name}, VersionArgs: []string{"version"}})
	if !st.Present || st.Version == "" {
		t.Fatalf("go not detected with version: %+v", st)
	}
}

func TestCheckOtherOS(t *testing.T) {
	other := "windows"
	if runtime.GOOS == "windows" {
		other = "linux"
	}
	if st := Check(context.Background(), Dep{Name: "x", OS: other}); st.Result != "unsupported" {
		t.Fatalf("got %+v", st)
	}
}

func TestExpandWinVars(t *testing.T) {
	for in, want := range map[string]string{
		`%A%\b`:     `${A}\b`,
		`x%A%y%B%z`: `x${A}y${B}z`,
		`no vars`:   `no vars`,
		`50% off`:   `50% off`,
	} {
		if got := expandWinVars(in); got != want {
			t.Errorf("expandWinVars(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScanLinesCR(t *testing.T) {
	data := []byte("a\rb\nc")
	var got []string
	for len(data) > 0 {
		n, tok, _ := scanLinesCR(data, true)
		got = append(got, string(tok))
		data = data[n:]
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("got %q", got)
	}
}
