package platformlife

// enroll.go — the line the root command of step 2 prints (pve.EnrollScript) and the operator pastes into the console:
// "PC-ENROLL:" plus base64 of "1:<installation id>:<unix time on the node>:<capi secret>:<csi secret>". It carries the
// secrets of the tokens that command made, so no secret is copied by hand; a token the command kept has an empty field.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// Errors of an enrollment line. None of them quotes the line: it holds secrets.
var (
	ErrEnrollMalformed = errors.New("this is not the PC-ENROLL line the root command of step 2 prints: copy the whole line, from PC-ENROLL: to its end")
	ErrEnrollOther     = errors.New("this PC-ENROLL line is of another installation: run the root command shown in step 2 of this installation")
	// ErrEnrollReplayed: the line, or a newer one, was stored before. The secrets stored are the newer ones.
	ErrEnrollReplayed = errors.New("this PC-ENROLL line was stored already, or a newer one was: nothing was changed. For new secrets run the root command of step 2 again")
)

var tokenSecretRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Enrollment is a parsed line: the tokens it carries (nil for one it does not) and when the node printed it.
type Enrollment struct {
	CAPI, CSI *TokenCredential
	Issued    int64
}

// ParseEnrollLine reads a pasted line for installationID. White space anywhere is dropped: a terminal may wrap the
// line. It answers ErrEnrollMalformed or ErrEnrollOther; whether the line was used before is the store's to say
// (Store.SaveEnrollment).
func ParseEnrollLine(line, installationID string) (*Enrollment, error) {
	line = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, line)
	b64, ok := strings.CutPrefix(line, pve.EnrollPrefix)
	if !ok {
		return nil, ErrEnrollMalformed
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, ErrEnrollMalformed
	}
	f := strings.Split(string(raw), ":")
	if len(f) != 5 || f[0] != "1" || f[1] == "" {
		return nil, ErrEnrollMalformed
	}
	issued, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || issued <= 0 {
		return nil, ErrEnrollMalformed
	}
	e := &Enrollment{Issued: issued}
	for _, t := range []struct {
		secret string
		id     string
		to     **TokenCredential
	}{{f[3], pve.CAPITokenID, &e.CAPI}, {f[4], pve.CSITokenID, &e.CSI}} {
		switch {
		case t.secret == "":
		case !tokenSecretRE.MatchString(t.secret):
			return nil, ErrEnrollMalformed
		default:
			*t.to = &TokenCredential{TokenID: t.id, Secret: t.secret}
		}
	}
	if e.CAPI == nil && e.CSI == nil {
		return nil, ErrEnrollMalformed
	}
	if f[1] != installationID {
		return nil, ErrEnrollOther
	}
	return e, nil
}

// String keeps a secret out of any log or error that prints an Enrollment.
func (e Enrollment) String() string { return fmt.Sprintf("enrollment issued %d", e.Issued) }
