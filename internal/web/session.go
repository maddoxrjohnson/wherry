// The session: a token login that mints a cookie, and the check every
// page runs first.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maddoxrjohnson/wherry/internal/ratelimit"
)

// withSession sends a visitor without a valid session to the login page.
func (s *Server) withSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.loggedIn(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, c.Value)
		return false
	}
	return true
}

// login checks the token in constant time and starts a session. Expired
// sessions are swept here, the one place the map grows. Every attempt
// counts against the client's limit, before the form is read.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.logins.Allow(ratelimit.ClientIP(r.RemoteAddr), time.Now()) {
		s.log.Warn("dashboard login rate limited", "remote", r.RemoteAddr)
		w.Header().Set("Retry-After", strconv.Itoa(int(loginRefill/time.Second)))
		s.render(w, http.StatusTooManyRequests, "login", map[string]any{"Error": "too many attempts, try again later"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	presented := strings.TrimSpace(r.PostFormValue("token"))
	if len(s.token) == 0 || subtle.ConstantTimeCompare([]byte(presented), s.token) != 1 {
		s.log.Warn("dashboard login refused", "remote", r.RemoteAddr)
		s.render(w, http.StatusUnauthorized, "login", map[string]any{"Error": "wrong token"})
		return
	}
	id := rand.Text()
	now := time.Now()
	s.mu.Lock()
	for k, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = now.Add(sessionTTL)
	s.mu.Unlock()
	// No Domain attribute: the cookie stays on this host and never reaches
	// the apps, whose code is not trusted with the session, on subdomains.
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL / time.Second)})
	s.log.Info("dashboard login", "remote", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// untilLoggedOut returns a context that also ends when r's session does,
// and a stop function that must be called when the request is done; it
// returns once the watcher has, so nothing touches r after the handler.
func (s *Server) untilLoggedOut(r *http.Request) (context.Context, func()) {
	ctx, cancel := context.WithCancel(r.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(s.sessionCheck)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if !s.loggedIn(r) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.loggedIn(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", nil)
}
