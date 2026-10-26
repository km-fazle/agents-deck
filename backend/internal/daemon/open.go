package daemon

import (
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
)

// pageURL checks a link the deck is about to open. Only http(s), and never
// one that carries a username or password.
func pageURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("this agent has no page to open")
	}
	return u.String(), nil
}

// openPage opens a link in the browser. On a Mac that is /usr/bin/open.
func openPage(raw string) error {
	u, err := pageURL(raw)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return exec.Command("/usr/bin/open", u).Run()
	}
	if lp, err := exec.LookPath("xdg-open"); err == nil {
		return exec.Command(lp, u).Run()
	}
	return errors.New("can't open a browser from here")
}
