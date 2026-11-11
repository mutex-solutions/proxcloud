package projectinvite

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestInviteRequestCredentialLoggingRedacted(t *testing.T) {
	v := InviteRequest{InviteID: "opaque", Email: "one@example.invalid", ProjectName: "PRIVATE_PROJECT", JoinURL: "https://console.example.invalid/?invite-token=PRIVATE_TOKEN"}
	var b bytes.Buffer
	slog.New(slog.NewJSONHandler(&b, nil)).Info("prepare", "request", v)
	public := b.String() + fmt.Sprintf("%v %+v %#v", v, v, v)
	for _, private := range []string{"PRIVATE_", "one@example.invalid", "invite-token", "JoinURL"} {
		if strings.Contains(public, private) {
			t.Fatal("credential/PII logging leaked")
		}
	}
}
