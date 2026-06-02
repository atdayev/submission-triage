package email

import "testing"

func TestIsLocalhost(t *testing.T) {
	for _, h := range []string{"localhost", "127.0.0.1", "::1", "LOCALHOST"} {
		if !isLocalhost(h) {
			t.Errorf("%q should be treated as localhost", h)
		}
	}
	for _, h := range []string{"smtp.gmail.com", "10.0.0.1", "example.com", ""} {
		if isLocalhost(h) {
			t.Errorf("%q should not be treated as localhost (remote requires STARTTLS)", h)
		}
	}
}
