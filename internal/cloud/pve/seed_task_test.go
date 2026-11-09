package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSeedTaskIdentityRejectsUnknownSuccess(t *testing.T) {
	for _, data := range []any{nil, "", "not-a-task", map[string]any{"unexpected": true}} {
		t.Run("response", func(t *testing.T) {
			mutations := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mutations++
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer srv.Close()
			c, err := New(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.UploadISO(context.Background(), "n1", "iso", "safe.iso", bytes.Repeat([]byte("x"), 512)); err == nil {
				t.Fatal("upload without known task accepted")
			}
			if err := c.DeleteStorageVolume(context.Background(), "n1", "iso", "iso/safe.iso"); err == nil {
				t.Fatal("delete without known task accepted")
			}
			if mutations != 2 {
				t.Fatal("unknown task caused compensation or status polling")
			}
		})
	}
	if err := seedTaskID("UPID:pve1:0003C4D1:1A2B3C4D:65F0A1B2:qmcreate:101:root@pam!proxcloud:", "pve1"); err != nil {
		t.Fatal(err)
	}
}
