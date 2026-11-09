//go:build live

package truenas

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TN_URL, TN_USER, TN_KEY, TN_METHODS (JSON list of {method, params}) drive a real NAS.
func TestLiveCalls(t *testing.T) {
	c, err := New(Config{URL: os.Getenv("TN_URL"), Username: os.Getenv("TN_USER"), APIKey: os.Getenv("TN_KEY"), InsecureTLS: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var calls []struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	if err := json.Unmarshal([]byte(os.Getenv("TN_METHODS")), &calls); err != nil {
		t.Fatal(err)
	}
	for _, m := range calls {
		var out any
		err := c.Call(context.Background(), m.Method, m.Params, &out)
		raw, _ := json.Marshal(out)
		if len(raw) > 1200 {
			raw = raw[:1200]
		}
		fmt.Printf("%s -> err=%v %s\n", m.Method, err, raw)
	}
}
