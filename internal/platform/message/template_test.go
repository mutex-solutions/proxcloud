package message

import "testing"

func TestRenderMustache(t *testing.T) {
	body := `<p>Dear {{fullName}},</p><p>Balance: {{balance}} {{currency}}</p><p>{{businessName}}</p>`
	got := renderMustache(body, map[string]any{"fullName": "Ada Lovelace", "balance": 12.5, "currency": "USD", "businessName": "ProxCloud"})
	want := `<p>Dear Ada Lovelace,</p><p>Balance: 12.5 USD</p><p>ProxCloud</p>`
	if got != want {
		t.Fatalf("render:\n got=%q\nwant=%q", got, want)
	}
}

func TestRenderMissingVarIsEmpty(t *testing.T) {
	got := renderMustache(`Hi {{name}}{{missing}}!`, map[string]any{"name": "X"})
	if got != "Hi X!" {
		t.Fatalf("missing var not empty: %q", got)
	}
}

func TestRenderEscapesValues(t *testing.T) {
	got := renderMustache(`<p>{{x}}</p>`, map[string]any{"x": "<b>&hi</b>"})
	if got != `<p>&lt;b&gt;&amp;hi&lt;/b&gt;</p>` {
		t.Fatalf("not escaped: %q", got)
	}
}

func TestSystemTemplatesComplete(t *testing.T) {
	tmpls := SystemTemplates()
	if len(tmpls) != 18 {
		t.Fatalf("system templates = %d, want 18", len(tmpls))
	}
	for _, x := range tmpls {
		if x.Key == "" || x.MessageTitle == "" || x.MessageBody == "" || !x.SystemTemplate {
			t.Fatalf("incomplete template: %+v", x.Key)
		}
	}
}
