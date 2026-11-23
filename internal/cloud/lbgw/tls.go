package lbgw

// tls.go: TLS that ends on the balancer, with the certificate of a Secret the Service names.
//
//	proxcloud.io/lb-tls-secret: web-tls[,other-tls]   Secrets of type kubernetes.io/tls in the Service's namespace
//	proxcloud.io/lb-tls-ports: "443,8443"             the TCP ports that end TLS (absent: every TCP port)
//	proxcloud.io/lb-backend-protocol: tcp             members are spoken plain TCP (absent or http: HTTP)
//
// The Secrets are the one thing of a Service the portal cannot read, so the controller reads them: by name, in the
// Service's own namespace, of that one type, and nothing else of the kind (cmd/lb-controller). It does not send a
// key with the state. It sends, per Service, the Secrets' names and a hash of each one's certificate and key
// (CertRef); the portal answers with the hashes it holds no certificate for (Answer.NeedCertificates), and only those
// are sent, once, in the next request (Request.CertificateData). A Secret that did not change is never sent again,
// and the state the portal stores holds the hashes alone.
//
// What the portal does with a certificate (checks it as an upload, seals it under the cluster's balancer, hands it to
// the appliances as a file named by its fingerprint) is internal/platform/project/cloud_lb_cluster_tls.go.
//
// A Service that asks for TLS is never served without it: with a Secret that cannot be used it stays as the balancer
// last served it (the Event says why), and one the balancer never served is refused. An annotation that is there
// and empty asks for TLS too, and is held the same way.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	KnobTLSSecret       = "tls-secret"
	KnobTLSPorts        = "tls-ports"
	KnobBackendProtocol = "backend-protocol"

	// TLSSecretType is the one type of Secret a Service may name.
	TLSSecretType = "kubernetes.io/tls"
	// MaxServiceCerts is how many Secrets a Service names (the first one answers a client that asks for no name, the
	// others are chosen by the name the client asks for), MaxClusterCerts how many different certificates the
	// Services of a cluster name together.
	MaxServiceCerts = 4
	MaxClusterCerts = 16
	// MaxCertBundle is what a Secret's certificate and key hold together (pve.LBMaxCertBundle, the same figure).
	MaxCertBundle = 16 << 10
	// maxCertBody is what the certificates of one request may add to its body: each one's 16 KiB, the escapes of
	// its line ends and its field names.
	maxCertBody = MaxClusterCerts * (MaxCertBundle + 2<<10)

	ReasonInvalidCertificate = "InvalidCertificate"
	// ReasonTLSNotAvailable: the portal has no key to seal a certificate with, so it takes none.
	ReasonTLSNotAvailable = "TLSNotAvailable"
	// ReasonCertificateExpired: a certificate that is served and is past its last day.
	ReasonCertificateExpired = "CertificateExpired"
)

// Why the controller could not use a Secret (CertRef.Problem). The text a Service's owner reads is made here from
// the code (CertProblem): nothing the controller wrote reaches a message.
const (
	CertNotFound   = "NotFound"
	CertWrongType  = "WrongType"
	CertIncomplete = "Incomplete"
	CertNotText    = "NotText"
	CertTooLarge   = "TooLarge"
	CertForbidden  = "Forbidden"
	CertUnreadable = "Unreadable"
	CertBadNames   = "BadNames"
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CertRef is one Secret a Service names: its name and SecretHash of what it holds, or why it cannot be used.
type CertRef struct {
	Secret  string `json:"secret,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// CertificateData is a Secret's tls.crt and tls.key, as the Secret holds them. It is a secret: it crosses to the
// portal once, when the portal asked for its hash, and is printed nowhere.
type CertificateData struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privateKey"`
}

// String and LogValue keep a certificate out of a log line whatever prints it.
func (CertificateData) String() string       { return "[certificate redacted]" }
func (CertificateData) GoString() string     { return "[certificate redacted]" }
func (CertificateData) LogValue() slog.Value { return slog.StringValue("[certificate redacted]") }

// SecretHash names a Secret's content: the SHA-256 of its certificate and its key. A new certificate and a new key
// each give another hash, which is how the portal learns of a rotation. The certificate's length goes first, so no
// two pairs give the same bytes.
func SecretHash(certificate, privateKey []byte) string {
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(certificate)))
	h.Write(n[:])
	h.Write(certificate)
	h.Write(privateKey)
	return hex.EncodeToString(h.Sum(nil))
}

// WantsTLS: the Service asks for TLS on the balancer, by its annotation or by the certificates sent with it. The
// annotation counts when it is there, whatever it holds: an empty value (a template that filled nothing in) still
// marks ports that are meant for TLS, and CertProblem holds such a Service back.
func WantsTLS(s *Service) bool {
	_, set := s.Annotations[OwnPrefix+KnobTLSSecret]
	return len(s.Certificates) > 0 || set
}

// TLSSecretNames reads the value of proxcloud.io/lb-tls-secret: one to MaxServiceCerts names of Secrets, between
// commas. A name is a Secret's name and nothing more: no namespace can be given, so the Secret is one of the
// Service's own namespace. ok false: the value names nothing that can be read.
func TLSSecretNames(value string) (names []string, ok bool) {
	if len(value) > MaxAnnotationValue {
		return nil, false
	}
	for _, n := range strings.Split(value, ",") {
		n = strings.TrimSpace(n)
		if len(n) > maxNameLen || !nameRe.MatchString(n) || slices.Contains(names, n) {
			return nil, false
		}
		names = append(names, n)
	}
	return names, len(names) <= MaxServiceCerts
}

// checkCertRefs is the gateway's check of the form of a Service's certificates.
func checkCertRefs(refs []CertRef) (reason, message string) {
	if len(refs) > MaxServiceCerts {
		return ReasonInvalid, fmt.Sprintf("A Service names at most %d TLS Secrets", MaxServiceCerts)
	}
	for _, c := range refs {
		named := c.Secret != "" && len(c.Secret) <= maxNameLen && nameRe.MatchString(c.Secret)
		switch {
		case c.Problem == "" && (!named || !sha256Re.MatchString(c.SHA256)):
			return ReasonInvalid, "A certificate of a Service is named by its Secret and the SHA-256 of its content"
		case c.Problem != "" && (c.SHA256 != "" || (c.Secret != "" && !named) || len(c.Problem) > 32):
			return ReasonInvalid, "A Secret that cannot be used is named with the reason and nothing else"
		}
	}
	return "", ""
}

// CertProblem says why the Service's TLS cannot be set up from what its controller sent; "" when it can, or when the
// Service asks for none.
func CertProblem(s *Service) string {
	name := OwnPrefix + KnobTLSSecret
	mustName := fmt.Sprintf("must name one to %d Secrets of the Service's namespace, by name, between commas", MaxServiceCerts)
	if v, set := s.Annotations[name]; set && strings.TrimSpace(v) == "" {
		return name + " is empty: it " + mustName // whatever the controller sent with it
	}
	if len(s.Certificates) == 0 {
		if !WantsTLS(s) {
			return ""
		}
		return name + " is set, and the cluster's load balancer controller sent no certificate for it: upgrade the cluster"
	}
	for _, c := range s.Certificates {
		secret := "Secret " + s.Namespace + "/" + c.Secret
		switch c.Problem {
		case "":
		case CertBadNames:
			return name + " " + mustName
		case CertNotFound:
			return secret + " does not exist"
		case CertWrongType:
			return secret + " is not of type " + TLSSecretType
		case CertIncomplete:
			return secret + " does not hold tls.crt and tls.key"
		case CertNotText:
			return secret + " does not hold PEM text in tls.crt and tls.key"
		case CertTooLarge:
			return fmt.Sprintf("%s holds more than %d KiB of certificate and key", secret, MaxCertBundle>>10)
		case CertForbidden:
			return secret + " may not be read by the cluster's load balancer controller: it needs to get Secrets by name"
		default:
			return secret + " could not be read"
		}
	}
	return ""
}

// tlsOptions reads the two annotations the portal reads of a Service that ends TLS. ports are the TCP ports that end
// it (nil: every TCP port), tcp says that the members are spoken plain TCP.
func tlsOptions(s *Service, ann map[string]string, bad func(name, value, want string)) (ports []int, tcp bool) {
	if v, name, ok := Annotation(ann, KnobBackendProtocol); ok {
		switch strings.ToLower(v) {
		case "tcp":
			tcp = true
		case "http", "":
		default:
			bad(name, v, "http or tcp")
		}
	}
	if v, name, ok := Annotation(ann, KnobTLSPorts); ok && v != "" {
		for _, term := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(term))
			if err != nil || !slices.ContainsFunc(s.Ports, func(p Port) bool { return p.Port == n && p.Protocol != "UDP" }) {
				// Every TCP port ends TLS then, which is the default: a port that was to end TLS is never left
				// plain over a mistyped number.
				bad(name, v, "a list of TCP ports of the Service")
				return nil, tcp
			}
			if !slices.Contains(ports, n) {
				ports = append(ports, n)
			}
		}
	}
	return ports, tcp
}

// EndsTLS: port p of Service s ends TLS on the balancer. Only a TCP port does, and only of a Service that came with
// its certificates.
func (o Options) EndsTLS(s *Service, p Port) bool {
	return len(s.Certificates) > 0 && p.Protocol != "UDP" && (o.TLSPorts == nil || slices.Contains(o.TLSPorts, p.Port))
}
