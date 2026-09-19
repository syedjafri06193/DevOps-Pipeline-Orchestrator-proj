package redact

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestKnownLiteralsAreScrubbed(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, "hunter2-the-real-password")
	if _, err := io.WriteString(r, "connecting with hunter2-the-real-password\n"); err != nil {
		t.Fatal(err)
	}
	_ = r.Flush()
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("the secret survived: %q", buf.String())
	}
	if !strings.Contains(buf.String(), Placeholder) {
		t.Errorf("want a placeholder: %q", buf.String())
	}
}

func TestPatternsCatchUnregisteredCredentials(t *testing.T) {
	// The literals from the secret store are the primary defence; these catch
	// the ones nobody told us about.
	for name, secret := range map[string]string{
		"aws key":    "AKIAIOSFODNN7EXAMPLE",
		"slack bot":  "xoxb-1234567890-abcdefghijkl",
		"slack app":  "xapp-1-A012345-0123456789-abcdef",
		"github pat": "ghp_abcdefghijklmnopqrstuvwxyz0123",
		"gitlab pat": "glpat-abcdefghijklmnopqrst",
		"jwt":        "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dQw4w9WgXcQdQw4w9WgXcQ",
		"assignment": "password: correct-horse-battery",
	} {
		t.Run(name, func(t *testing.T) {
			got := String("output before\n" + secret + "\noutput after\n")
			if strings.Contains(got, secret) {
				t.Errorf("%s survived redaction: %q", name, got)
			}
			if !strings.Contains(got, "output before") || !strings.Contains(got, "output after") {
				t.Errorf("surrounding output was lost: %q", got)
			}
		})
	}
}

func TestAPrivateKeyBlockIsRemovedEntirely(t *testing.T) {
	key := `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAxGbz1z
abcdefghijklmnopqrstuv
-----END RSA PRIVATE KEY-----`
	got := String("deploying\n" + key + "\ndone\n")
	if strings.Contains(got, "MIIEow") {
		t.Errorf("key body survived: %q", got)
	}
	if !strings.Contains(got, "done") {
		t.Errorf("output after the key was lost: %q", got)
	}
}

func TestWriteReportsTheOriginalLength(t *testing.T) {
	// The document calls this out specifically: "report len(p), not the
	// redacted length, or callers that check n != len(p) will see spurious
	// short-write errors."
	var buf bytes.Buffer
	r := New(&buf, "a-very-secret-value")
	in := []byte("token a-very-secret-value here\n")
	n, err := r.Write(in)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(in) {
		t.Fatalf("Write returned %d, want %d -- callers will see a short write", n, len(in))
	}
}

func TestIoCopyDoesNotSeeAShortWrite(t *testing.T) {
	// The consequence of the rule above, exercised through the standard
	// library rather than asserted directly.
	var buf bytes.Buffer
	r := New(&buf, "a-very-secret-value")
	src := strings.NewReader(strings.Repeat("token a-very-secret-value\n", 50))
	if _, err := io.Copy(r, src); err != nil {
		t.Fatalf("io.Copy failed, which is what a wrong return length causes: %v", err)
	}
	_ = r.Flush()
	if strings.Contains(buf.String(), "a-very-secret-value") {
		t.Error("secret survived")
	}
}

func TestASecretSplitAcrossTwoWritesIsStillCaught(t *testing.T) {
	// Provider output arrives in whatever chunks the network produced. A
	// redactor that looks at one call at a time misses exactly the secrets
	// that matter.
	var buf bytes.Buffer
	r := New(&buf, "AKIAIOSFODNN7EXAMPLE")
	if _, err := io.WriteString(r, "using key AKIAIOSF"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(r, "ODNN7EXAMPLE to connect\n"); err != nil {
		t.Fatal(err)
	}
	_ = r.Flush()
	if strings.Contains(buf.String(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("a split secret survived: %q", buf.String())
	}
}

func TestFlushEmitsTheTail(t *testing.T) {
	// Without Flush the last partial line of a deploy log is silently lost,
	// and that is usually the line that says what went wrong.
	var buf bytes.Buffer
	r := New(&buf)
	if _, err := io.WriteString(r, "no trailing newline"); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Log("held back, as expected")
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "no trailing newline" {
		t.Errorf("after flush = %q", got)
	}
}

func TestShortLiteralsAreIgnored(t *testing.T) {
	// An empty or one-character environment variable registered as a literal
	// would otherwise redact the entire log, silently.
	var buf bytes.Buffer
	r := New(&buf, "", "a", "ok")
	if _, err := io.WriteString(r, "a perfectly ordinary log line\n"); err != nil {
		t.Fatal(err)
	}
	_ = r.Flush()
	if strings.Contains(buf.String(), Placeholder) {
		t.Errorf("a short literal destroyed the log: %q", buf.String())
	}
}

func TestCountingLetsYouLogThatSomethingWasScrubbed(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, "shhh-this-is-secret")
	_, _ = io.WriteString(r, "one shhh-this-is-secret two shhh-this-is-secret\n")
	_ = r.Flush()
	if r.Count != 2 {
		t.Errorf("Count = %d, want 2", r.Count)
	}
}

func TestOrdinaryOutputIsUnchanged(t *testing.T) {
	// Over-redaction makes the deploy log useless, which is its own failure.
	in := "deploying v2 to web/prod\ninstance 1/3: ok\ninstance 2/3: ok\ninstance 3/3: ok\ndeployed\n"
	if got := String(in); got != in {
		t.Errorf("ordinary output was altered:\n%q\n%q", in, got)
	}
}
