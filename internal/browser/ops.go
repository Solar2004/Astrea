package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Navigate loads url and waits briefly for it to settle, returning the final
// URL and title.
func (b *Browser) Navigate(ctx context.Context, url string) (map[string]any, error) {
	c, err := b.ensure(ctx)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(url, "://") && !strings.HasPrefix(url, "about:") {
		url = "https://" + url
	}
	if _, err := c.call(ctx, "Page.navigate", map[string]any{"url": url}); err != nil {
		return nil, err
	}
	b.settle(ctx, c)
	return b.info(ctx, c)
}

// settle waits until the document is interactive or a short timeout.
func (b *Browser) settle(ctx context.Context, c *conn) {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		v, err := evalRaw(ctx, c, `document.readyState`)
		if err == nil && (string(v) == `"complete"` || string(v) == `"interactive"`) {
			time.Sleep(300 * time.Millisecond)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (b *Browser) info(ctx context.Context, c *conn) (map[string]any, error) {
	url, _ := evalString(ctx, c, `location.href`)
	title, _ := evalString(ctx, c, `document.title`)
	return map[string]any{"url": url, "title": title}, nil
}

// Info returns the current page URL and title.
func (b *Browser) Info(ctx context.Context) (map[string]any, error) {
	c, err := b.ensure(ctx)
	if err != nil {
		return nil, err
	}
	return b.info(ctx, c)
}

// readScript extracts the visible text of the page, trimmed. It walks the DOM
// skipping script/style/hidden nodes so the AI gets readable content, not markup.
const readScript = `(function(max){
  function visible(el){ const s=getComputedStyle(el); return s && s.display!=='none' && s.visibility!=='hidden'; }
  function walk(node, out){
    for(const n of node.childNodes){
      if(out.len>max) return;
      if(n.nodeType===3){ const t=n.textContent.replace(/\s+/g,' '); if(t.trim()){ out.parts.push(t); out.len+=t.length; } }
      else if(n.nodeType===1){ const tag=n.tagName.toLowerCase();
        if(tag==='script'||tag==='style'||tag==='noscript'||tag==='svg') continue;
        if(!visible(n)) continue;
        walk(n, out);
        if(['p','div','br','li','tr','h1','h2','h3','h4','section','article'].includes(tag)) out.parts.push('\n');
      }
    }
  }
  const out={parts:[],len:0}; walk(document.body||document.documentElement, out);
  return out.parts.join(' ').replace(/ \n /g,'\n').replace(/\n{3,}/g,'\n\n').trim();
})`

// Read returns the page's visible text (up to max characters).
func (b *Browser) Read(ctx context.Context, max int) (map[string]any, error) {
	c, err := b.ensure(ctx)
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 20000
	}
	text, err := evalString(ctx, c, fmt.Sprintf("%s(%d)", readScript, max))
	if err != nil {
		return nil, err
	}
	truncated := false
	if len([]rune(text)) > max {
		text = string([]rune(text)[:max])
		truncated = true
	}
	res, _ := b.info(ctx, c)
	res["text"] = text
	res["truncated"] = truncated
	return res, nil
}

// clickScript finds the first element matching selector-or-text and clicks it.
const actScript = `(function(sel, text, value, action){
  let el=null;
  if(sel){ el=document.querySelector(sel); }
  else if(text){
    const all=[...document.querySelectorAll('a,button,input,[role=button],[onclick],label,summary,li,span,div')];
    el=all.find(e=>{
      const t=(e.innerText||e.value||e.getAttribute('aria-label')||'').trim();
      return t===text;
    }) || all.find(e=>((e.innerText||e.value||e.getAttribute('aria-label')||'').trim().includes(text)));
  }
  if(!el) return {ok:false, error:'no element matched'};
  el.scrollIntoView({block:'center'});
  if(action==='fill'){
    el.focus();
    const setter=Object.getOwnPropertyDescriptor(el.__proto__,'value');
    if(setter&&setter.set) setter.set.call(el, value); else el.value=value;
    el.dispatchEvent(new Event('input',{bubbles:true}));
    el.dispatchEvent(new Event('change',{bubbles:true}));
    return {ok:true, tag:el.tagName.toLowerCase()};
  }
  el.click();
  return {ok:true, tag:el.tagName.toLowerCase(), text:(el.innerText||el.value||'').slice(0,80)};
})`

// Act clicks or fills an element identified by a CSS selector or exact/partial
// visible text. action is "click" or "fill".
func (b *Browser) Act(ctx context.Context, action, selector, text, value string) (map[string]any, error) {
	c, err := b.ensure(ctx)
	if err != nil {
		return nil, err
	}
	call := fmt.Sprintf("%s(%s,%s,%s,%s)", actScript,
		jsonStr(selector), jsonStr(text), jsonStr(value), jsonStr(action))
	raw, err := evalRaw(ctx, c, call)
	if err != nil {
		return nil, err
	}
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Tag   string `json:"tag"`
		Text  string `json:"text"`
	}
	_ = json.Unmarshal(raw, &r)
	if !r.OK {
		return nil, fmt.Errorf("%s: %s", action, r.Error)
	}
	if action == "click" {
		b.settle(ctx, c)
	}
	return map[string]any{"ok": true, "tag": r.Tag, "matched": r.Text}, nil
}

// Eval runs arbitrary JS in the page and returns its JSON result.
func (b *Browser) Eval(ctx context.Context, expr string) (json.RawMessage, error) {
	c, err := b.ensure(ctx)
	if err != nil {
		return nil, err
	}
	return evalRaw(ctx, c, expr)
}

// Screenshot captures the page as PNG base64.
func (b *Browser) Screenshot(ctx context.Context) (string, error) {
	c, err := b.ensure(ctx)
	if err != nil {
		return "", err
	}
	raw, err := c.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return "", err
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	return r.Data, nil
}

// --- Runtime.evaluate helpers ---

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// evalRaw evaluates expr and returns the JSON value (CDP returns it by value).
func evalRaw(ctx context.Context, c *conn, expr string) (json.RawMessage, error) {
	raw, err := c.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return nil, err
	}
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.Exception != nil {
		return nil, fmt.Errorf("page error: %s", r.Exception.Text)
	}
	if len(r.Result.Value) == 0 {
		return json.RawMessage("null"), nil
	}
	return r.Result.Value, nil
}

func evalString(ctx context.Context, c *conn, expr string) (string, error) {
	raw, err := evalRaw(ctx, c, expr)
	if err != nil {
		return "", err
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	return string(raw), nil
}
