// Command authorize runs a one-time Google OAuth consent flow on a loopback
// redirect and prints the refresh token to paste into MAIL_OAUTH_REFRESH_TOKEN.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/oauth2"

	"github.com/atdayev/submission-triage/internal/infrastructure/oauth"
)

const (
	consentTimeout  = 5 * time.Minute
	exchangeTimeout = 30 * time.Second
)

func main() {
	clientID := os.Getenv("MAIL_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("MAIL_OAUTH_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		log.Fatal("set MAIL_OAUTH_CLIENT_ID and MAIL_OAUTH_CLIENT_SECRET from your Google desktop OAuth client")
	}
	if err := run(clientID, clientSecret); err != nil {
		log.Fatal(err)
	}
}

func run(clientID, clientSecret string) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open loopback listener: %w", err)
	}
	defer ln.Close()

	cfg := oauth.GoogleConsentConfig(clientID, clientSecret, fmt.Sprintf("http://%s/callback", ln.Addr().String()))
	state, err := randomState()
	if err != nil {
		return err
	}
	// access_type=offline + prompt=consent forces a refresh token even on re-consent
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("state") != state:
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- errors.New("state mismatch (possible CSRF); aborting")
		case q.Get("error") != "":
			http.Error(w, q.Get("error"), http.StatusBadRequest)
			errCh <- fmt.Errorf("consent denied: %s", q.Get("error"))
		default:
			fmt.Fprintln(w, "Authorization received. You can close this tab and return to the terminal.")
			codeCh <- q.Get("code")
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	fmt.Println("Opening the Google consent page in your browser. If it does not open, visit:")
	fmt.Print("\n  " + authURL + "\n\n")
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(consentTimeout):
		return errors.New("timed out waiting for consent")
	}

	ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
	defer cancel()
	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}
	if tok.RefreshToken == "" {
		return errors.New("no refresh token returned; revoke prior access at https://myaccount.google.com/permissions and retry")
	}
	fmt.Print("\nSuccess. Paste this into your .env:\n\n")
	fmt.Println("  MAIL_OAUTH_REFRESH_TOKEN=" + tok.RefreshToken)
	return nil
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// openBrowser best-effort opens url; failure is non-fatal since the URL is printed.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd, args = "open", []string{url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
