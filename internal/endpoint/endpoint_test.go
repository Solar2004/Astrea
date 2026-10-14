package endpoint

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSplitName(t *testing.T) {
	cases := []struct{ path, name, rest string }{
		{"/dyn/ping", "ping", "/"},
		{"/dyn/api/v1/infer", "api", "/v1/infer"},
		{"/dyn/", "", ""},
		{"/other", "", ""},
	}
	for _, c := range cases {
		name, rest := splitName(c.path)
		if name != c.name || rest != c.rest {
			t.Errorf("splitName(%q) = (%q,%q); want (%q,%q)", c.path, name, rest, c.name, c.rest)
		}
	}
}

func TestStaticEndpoint(t *testing.T) {
	m := NewManager("")
	if err := m.Create(&Spec{Name: "hi", Kind: KindStatic, Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dyn/hi", nil))
	if rec.Body.String() != "hello" {
		t.Fatalf("got %q", rec.Body.String())
	}
	if !m.Delete("hi") {
		t.Fatal("delete should report true")
	}
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dyn/hi", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", rec.Code)
	}
}

func TestCreateValidation(t *testing.T) {
	m := NewManager("")
	if err := m.Create(&Spec{Name: "x", Kind: KindExec}); err == nil {
		t.Fatal("exec without command should error")
	}
	if err := m.Create(&Spec{Name: "y", Kind: "bogus"}); err == nil {
		t.Fatal("unknown kind should error")
	}
}

func TestProxyTargetValidation(t *testing.T) {
	m := NewManager("")
	for _, bad := range []string{"", "127.0.0.1:11434", "file:///etc/passwd", "http://"} {
		if err := m.Create(&Spec{Name: "p", Kind: KindProxy, Target: bad}); err == nil {
			t.Errorf("target %q should be rejected", bad)
		}
	}
	if err := m.Create(&Spec{Name: "p", Kind: KindProxy, Target: "http://127.0.0.1:11434"}); err != nil {
		t.Fatal(err)
	}
}
