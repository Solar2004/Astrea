package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Solar2004/Astrea/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRenderExtractsImages(t *testing.T) {
	res := &transport.CallResult{Result: json.RawMessage(`{"scale":0.5,"images":[{"mime_type":"image/jpeg","data":"aGk="}]}`)}
	out := render(res)
	if len(out) != 2 {
		t.Fatalf("want image + json, got %d items", len(out))
	}
	img, ok := out[0].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/jpeg" || string(img.Data) != "hi" {
		t.Fatalf("first item should be the decoded image, got %#v", out[0])
	}
	txt := out[1].(*mcp.TextContent).Text
	if !json.Valid([]byte(txt)) || strings.Contains(txt, "images") {
		t.Fatalf("json part should not repeat the image: %s", txt)
	}
}
