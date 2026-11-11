package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Users and sessions.
//
// Users live in <data>/users.json (mode 600). Passwords are never stored: only a salted
// PBKDF2-HMAC-SHA256 hash (600,000 iterations, OWASP 2023), so the file can't be turned
// back into passwords. On first start an admin is created (KUBELAMP_ADMIN_USER /
// KUBELAMP_ADMIN_PASSWORD, default admin/admin); after that the file is left alone.
// The admin creates the other users; everyone must set their own password at first login.
//
// Each user has their own folder <data>/users/<id>/ with their kubeconfigs and settings,
// and their own cluster connections: users never see each other's clusters.
//
// Sessions are random 256-bit IDs in an HttpOnly, SameSite=Strict cookie, kept in memory
// (only their SHA-256 is stored), expiring after 12 h idle or 7 days; a restart logs
// everyone out.

// pbkdfIter is the PBKDF2 work factor for new hashes (a var so tests can run faster;
// existing hashes keep their own count).
var pbkdfIter = 600_000

const (
	sessionCookie   = "kubelamp_session"
	sessionIdle     = 12 * time.Hour
	sessionMax      = 7 * 24 * time.Hour
	minPasswordLen  = 8
	maxPasswordLen  = 256
	defaultAdmin    = "admin"
	defaultAdminPwd = "admin"
)

type User struct {
	ID         string    `json:"id"`
	Username   string    `json:"username"`
	Role       string    `json:"role"` // admin | user
	Hash       string    `json:"hash"`
	MustChange bool      `json:"mustChange"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
}

func (u *User) Admin() bool { return u.Role == "admin" }

// UserView is what the browser sees (never the hash).
type UserView struct {
	ID         string    `json:"id"`
	Username   string    `json:"username"`
	Role       string    `json:"role"`
	MustChange bool      `json:"mustChange"`
	Created    time.Time `json:"created"`
}

func (u *User) view() UserView {
	return UserView{ID: u.ID, Username: u.Username, Role: u.Role, MustChange: u.MustChange, Created: u.Created}
}

type session struct {
	userID   string
	created  time.Time
	lastSeen time.Time
}

type failure struct {
	n     int
	until time.Time
}

type UserStore struct {
	dir       string
	mu        sync.Mutex
	users     []*User
	sessions  map[string]*session // sha256(session id) → session
	fails     map[string]*failure // login throttling, per username+client
	regs      map[string]*Registry
	inCluster *resolvedContext // given to admins only
	bootPaths []string         // -kubeconfig: loaded for the admin when they have none
}

// ---------------------------------------------------------------- passwords

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	u := make([]byte, hashLen)
	t := make([]byte, hashLen)
	for b := 1; b <= blocks; b++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(b))
		prf.Write(buf)
		u = prf.Sum(u[:0])
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	k := pbkdf2SHA256([]byte(pw), salt, pbkdfIter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdfIter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(k))
}

func checkPassword(hash, pw string) bool {
	p := strings.Split(hash, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1000 || iter > 10_000_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	want, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// a hash to check against for unknown usernames, so timing doesn't reveal which exist
var dummyHash = hashPassword("kubelamp-no-such-user")

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]{1,63}$`)

func validUsername(u string) error {
	if !usernameRe.MatchString(u) {
		return errors.New("username: 2–64 characters, letters, digits and . _ @ - (starting with a letter or digit)")
	}
	return nil
}

func validPassword(username, pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < minPasswordLen:
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	case len(pw) > maxPasswordLen:
		return fmt.Errorf("password must be at most %d characters", maxPasswordLen)
	case strings.EqualFold(pw, username):
		return errors.New("password can't be the same as the username")
	case strings.EqualFold(pw, defaultAdminPwd) || strings.EqualFold(pw, "password") || strings.EqualFold(pw, "kubelamp"):
		return errors.New("that password is too easy to guess")
	}
	return nil
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func sessKey(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------- store

func (s *UserStore) file() string { return filepath.Join(s.dir, "users.json") }

// UserDir is a user's private folder (kubeconfigs, settings).
func (s *UserStore) UserDir(u *User) string { return filepath.Join(s.dir, "users", u.ID) }

// openUserStore loads users.json, or creates it with the first admin.
func openUserStore(dataDir, adminUser, adminPwd string, ic *resolvedContext, bootPaths []string) (*UserStore, error) {
	s := &UserStore{dir: dataDir, sessions: map[string]*session{}, fails: map[string]*failure{}, regs: map[string]*Registry{},
		inCluster: ic, bootPaths: bootPaths}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.file())
	if err == nil {
		var f struct {
			Users []*User `json:"users"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("%s: %v", s.file(), err)
		}
		s.users = f.Users
		if len(s.users) == 0 {
			return nil, fmt.Errorf("%s has no users; delete it to create the admin again", s.file())
		}
		return s, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	// first start: create the admin
	if adminUser == "" {
		adminUser = defaultAdmin
	}
	if err := validUsername(adminUser); err != nil {
		return nil, fmt.Errorf("KUBELAMP_ADMIN_USER: %v", err)
	}
	mustChange := adminPwd == ""
	if adminPwd == "" {
		adminPwd = defaultAdminPwd
	} else if err := validPassword(adminUser, adminPwd); err != nil {
		return nil, fmt.Errorf("KUBELAMP_ADMIN_PASSWORD: %v", err)
	}
	now := time.Now().UTC()
	admin := &User{ID: randToken(8), Username: adminUser, Role: "admin", Hash: hashPassword(adminPwd), MustChange: mustChange, Created: now, Updated: now}
	s.users = []*User{admin}
	s.migrateSingleUser(admin)
	if err := s.save(); err != nil {
		return nil, err
	}
	if mustChange {
		log.Printf("created user %q with the default password %q: you must change it at first login", adminUser, defaultAdminPwd)
	} else {
		log.Printf("created user %q with the password from KUBELAMP_ADMIN_PASSWORD", adminUser)
	}
	return s, nil
}

// migrateSingleUser hands kubeconfigs added before users existed to the first admin.
func (s *UserStore) migrateSingleUser(admin *User) {
	old := loadSettings(s.dir)
	if len(old.Kubeconfig) == 0 && len(old.Access) == 0 {
		return
	}
	udir := s.UserDir(admin)
	_ = os.MkdirAll(filepath.Join(udir, "kubeconfigs"), 0o700)
	oldKC := filepath.Join(s.dir, "kubeconfigs")
	for i, p := range old.Kubeconfig {
		if rel, err := filepath.Rel(oldKC, p); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			np := filepath.Join(udir, "kubeconfigs", filepath.Base(p))
			if os.Rename(p, np) == nil {
				old.Kubeconfig[i] = np
			}
		}
	}
	saveSettings(udir, old)
	_ = os.Remove(settingsPath(s.dir))
	_ = os.Remove(oldKC)
	log.Printf("moved the existing kubeconfigs (%d) to user %q", len(old.Kubeconfig), admin.Username)
}

// save writes users.json atomically. Callers hold s.mu (or own s exclusively).
func (s *UserStore) save() error {
	b, err := json.MarshalIndent(map[string]any{"users": s.users}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file())
}

func (s *UserStore) byID(id string) *User {
	for _, u := range s.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func (s *UserStore) byName(name string) *User {
	for _, u := range s.users {
		if strings.EqualFold(u.Username, name) {
			return u
		}
	}
	return nil
}

func (s *UserStore) admins() int {
	n := 0
	for _, u := range s.users {
		if u.Admin() {
			n++
		}
	}
	return n
}

// Registry returns the user's own cluster connections, loading them on first use.
func (s *UserStore) Registry(u *User) *Registry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.regs[u.ID]; r != nil {
		return r
	}
	r := &Registry{}
	if u.Admin() {
		r.inCluster = s.inCluster
	}
	st := loadSettings(s.UserDir(u))
	paths := existingPaths(st.Kubeconfig)
	if len(paths) == 0 && u.Admin() && len(s.bootPaths) > 0 {
		paths = s.bootPaths
	}
	r.Load(paths, st.Access)
	s.regs[u.ID] = r
	return r
}

func (s *UserStore) dropRegistry(id string) {
	if r := s.regs[id]; r != nil {
		for _, c := range r.All() {
			c.Close()
		}
		delete(s.regs, id)
	}
}

// ---------------------------------------------------------------- sessions

func (s *UserStore) newSession(u *User) string {
	id := randToken(32)
	now := time.Now()
	s.mu.Lock()
	s.sessions[sessKey(id)] = &session{userID: u.ID, created: now, lastSeen: now}
	s.mu.Unlock()
	return id
}

// sessionUser returns the user for a session ID, or nil.
func (s *UserStore) sessionUser(id string) *User {
	if id == "" {
		return nil
	}
	k := sessKey(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[k]
	if ss == nil {
		return nil
	}
	now := time.Now()
	if now.Sub(ss.lastSeen) > sessionIdle || now.Sub(ss.created) > sessionMax {
		delete(s.sessions, k)
		return nil
	}
	u := s.byID(ss.userID)
	if u == nil {
		delete(s.sessions, k)
		return nil
	}
	ss.lastSeen = now
	cp := *u // a snapshot: handlers read it without holding the lock
	return &cp
}

func (s *UserStore) endSession(id string) {
	s.mu.Lock()
	delete(s.sessions, sessKey(id))
	s.mu.Unlock()
}

// endSessions logs a user out everywhere except keep (a session ID, or "").
func (s *UserStore) endSessions(userID, keep string) {
	kk := ""
	if keep != "" {
		kk = sessKey(keep)
	}
	s.mu.Lock()
	for k, ss := range s.sessions {
		if ss.userID == userID && k != kk {
			delete(s.sessions, k)
		}
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------- login throttling

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// throttled reports how long a login for this key must wait.
func (s *UserStore) throttled(key string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.fails[key]; f != nil {
		if d := time.Until(f.until); d > 0 {
			return d
		}
	}
	return 0
}

// failed records a wrong password: after 5 in a row the key is locked for 30 s,
// doubling each time up to 15 minutes.
func (s *UserStore) failed(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fails[key]
	if f == nil {
		f = &failure{}
		s.fails[key] = f
	}
	f.n++
	if f.n >= 5 {
		d := 30 * time.Second << min(f.n-5, 5)
		if d > 15*time.Minute {
			d = 15 * time.Minute
		}
		f.until = time.Now().Add(d)
	}
	if len(s.fails) > 10000 { // bound memory under a username-spraying attack
		for k, v := range s.fails {
			if time.Since(v.until) > time.Hour {
				delete(s.fails, k)
			}
		}
	}
}

func (s *UserStore) succeeded(key string) {
	s.mu.Lock()
	delete(s.fails, key)
	s.mu.Unlock()
}

// ---------------------------------------------------------------- request context

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
)

func reqUser(r *http.Request) *User {
	u, _ := r.Context().Value(ctxUser).(*User)
	return u
}

func reqSession(r *http.Request) string {
	s, _ := r.Context().Value(ctxSession).(string)
	return s
}

func withUser(r *http.Request, u *User, sid string) *http.Request {
	ctx := context.WithValue(r.Context(), ctxUser, u)
	ctx = context.WithValue(ctx, ctxSession, sid)
	return r.WithContext(ctx)
}

// R is the cluster registry for this request: the signed-in user's own, or the
// server-wide one when user accounts are not in use (tests).
func (s *Server) R(r *http.Request) *Registry {
	if s.users != nil {
		if u := reqUser(r); u != nil {
			return s.users.Registry(u)
		}
		return &Registry{}
	}
	return s.reg
}

// userDir is where this request's kubeconfigs and settings are kept.
func (s *Server) userDir(r *http.Request) string {
	if s.users != nil {
		if u := reqUser(r); u != nil {
			return s.users.UserDir(u)
		}
	}
	return s.dataDir
}

// isAdmin: user accounts off (tests) count as admin.
func (s *Server) isAdmin(r *http.Request) bool {
	if s.users == nil {
		return true
	}
	u := reqUser(r)
	return u != nil && u.Admin()
}

func reqUserID(r *http.Request) string {
	if u := reqUser(r); u != nil {
		return u.ID
	}
	return ""
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, Secure: secureRequest(r),
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

// ---------------------------------------------------------------- HTTP

func (s *Server) authRoutes(mux *http.ServeMux) {
	us := s.users
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
			writeErr(w, 400, errors.New("bad request"))
			return
		}
		name := strings.TrimSpace(req.Username)
		key := strings.ToLower(name) + "|" + clientIP(r)
		if d := us.throttled(key); d > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
			writeErr(w, 429, fmt.Errorf("too many failed attempts: try again in %s", d.Round(time.Second)))
			return
		}
		us.mu.Lock()
		u := us.byName(name)
		hash := dummyHash
		if u != nil {
			hash = u.Hash
		}
		us.mu.Unlock()
		ok := checkPassword(hash, req.Password) && u != nil && len(req.Password) <= maxPasswordLen
		if !ok {
			us.failed(key)
			log.Printf("login failed for %q from %s", name, clientIP(r))
			writeErr(w, 401, errors.New("wrong username or password"))
			return
		}
		us.succeeded(key)
		if old, err := r.Cookie(sessionCookie); err == nil {
			us.endSession(old.Value) // no session fixation: always a fresh ID
		}
		id := us.newSession(u)
		setSessionCookie(w, r, id, int(sessionMax.Seconds()))
		log.Printf("user %q signed in from %s", u.Username, clientIP(r))
		writeJSON(w, 200, map[string]any{"user": u.view()})
	})

	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			us.endSession(c.Value)
		}
		setSessionCookie(w, r, "", -1)
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"user": reqUser(r).view()})
	})

	// change your own password: {current, new}
	mux.HandleFunc("POST /api/auth/password", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Current string `json:"current"`
			New     string `json:"new"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
			writeErr(w, 400, errors.New("bad request"))
			return
		}
		u := reqUser(r)
		key := "pw|" + u.ID
		if d := us.throttled(key); d > 0 {
			writeErr(w, 429, fmt.Errorf("too many failed attempts: try again in %s", d.Round(time.Second)))
			return
		}
		us.mu.Lock()
		hash := ""
		if real := us.byID(u.ID); real != nil {
			hash = real.Hash
		}
		us.mu.Unlock()
		if !checkPassword(hash, req.Current) {
			us.failed(key)
			writeErr(w, 400, errors.New("current password is wrong"))
			return
		}
		us.succeeded(key)
		if err := validPassword(u.Username, req.New); err != nil {
			writeErr(w, 400, err)
			return
		}
		if req.New == req.Current {
			writeErr(w, 400, errors.New("choose a new password, different from the current one"))
			return
		}
		h := hashPassword(req.New)
		us.mu.Lock()
		real := us.byID(u.ID)
		if real == nil {
			us.mu.Unlock()
			writeErr(w, 404, errors.New("your account was deleted"))
			return
		}
		real.Hash, real.MustChange, real.Updated = h, false, time.Now().UTC()
		err := us.save()
		cp := *real
		us.mu.Unlock()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		u = &cp
		// sign out every other browser, and give this one a new session ID
		us.endSessions(u.ID, "")
		id := us.newSession(u)
		setSessionCookie(w, r, id, int(sessionMax.Seconds()))
		log.Printf("user %q changed their password", u.Username)
		writeJSON(w, 200, map[string]any{"user": u.view()})
	})

	// ---- admin: users
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !reqUser(r).Admin() {
				writeErr(w, 403, errors.New("only admins can manage users"))
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /api/users", admin(func(w http.ResponseWriter, r *http.Request) {
		us.mu.Lock()
		out := []UserView{}
		for _, u := range us.users {
			out = append(out, u.view())
		}
		us.mu.Unlock()
		writeJSON(w, 200, out)
	}))

	// create: {username, password, role}; the user must change the password at first login
	mux.HandleFunc("POST /api/users", admin(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
			writeErr(w, 400, errors.New("bad request"))
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if err := validUsername(req.Username); err != nil {
			writeErr(w, 400, err)
			return
		}
		if req.Role != "admin" {
			req.Role = "user"
		}
		if err := validPassword(req.Username, req.Password); err != nil {
			writeErr(w, 400, err)
			return
		}
		h := hashPassword(req.Password)
		now := time.Now().UTC()
		u := &User{ID: randToken(8), Username: req.Username, Role: req.Role, Hash: h, MustChange: true, Created: now, Updated: now}
		us.mu.Lock()
		if us.byName(req.Username) != nil {
			us.mu.Unlock()
			writeErr(w, 409, errors.New("a user with that name already exists"))
			return
		}
		us.users = append(us.users, u)
		err := us.save()
		if err != nil {
			us.users = us.users[:len(us.users)-1]
		}
		us.mu.Unlock()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		log.Printf("user %q created user %q (%s)", reqUser(r).Username, u.Username, u.Role)
		writeJSON(w, 200, u.view())
	}))

	// update: {role?, password?}: a new password is temporary (must change at next login)
	mux.HandleFunc("PUT /api/users/{id}", admin(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Role     string `json:"role"`
			Password string `json:"password"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
			writeErr(w, 400, errors.New("bad request"))
			return
		}
		h := ""
		us.mu.Lock()
		u := us.byID(r.PathValue("id"))
		var uname string
		if u != nil {
			uname = u.Username
		}
		us.mu.Unlock()
		if u == nil {
			writeErr(w, 404, errors.New("no such user"))
			return
		}
		if req.Password != "" {
			if err := validPassword(uname, req.Password); err != nil {
				writeErr(w, 400, err)
				return
			}
			h = hashPassword(req.Password)
		}
		us.mu.Lock()
		if req.Role != "" && req.Role != u.Role {
			if req.Role != "admin" && req.Role != "user" {
				us.mu.Unlock()
				writeErr(w, 400, errors.New("role must be admin or user"))
				return
			}
			if u.Admin() && us.admins() == 1 {
				us.mu.Unlock()
				writeErr(w, 400, errors.New("keep at least one admin"))
				return
			}
			u.Role = req.Role
			us.dropRegistry(u.ID) // in-cluster access follows the role
		}
		if h != "" {
			u.Hash, u.MustChange = h, true
		}
		u.Updated = time.Now().UTC()
		err := us.save()
		us.mu.Unlock()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if h != "" || req.Role != "" {
			us.endSessions(u.ID, keepIfSelf(r, u))
		}
		log.Printf("user %q updated user %q", reqUser(r).Username, u.Username)
		writeJSON(w, 200, u.view())
	}))

	mux.HandleFunc("DELETE /api/users/{id}", admin(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == reqUser(r).ID {
			writeErr(w, 400, errors.New("you can't delete yourself"))
			return
		}
		us.mu.Lock()
		u := us.byID(id)
		if u == nil {
			us.mu.Unlock()
			writeErr(w, 404, errors.New("no such user"))
			return
		}
		if u.Admin() && us.admins() == 1 {
			us.mu.Unlock()
			writeErr(w, 400, errors.New("keep at least one admin"))
			return
		}
		keep := us.users[:0:0]
		for _, x := range us.users {
			if x.ID != id {
				keep = append(keep, x)
			}
		}
		prev := us.users
		us.users = keep
		err := us.save()
		if err != nil {
			us.users = prev
		} else {
			us.dropRegistry(id)
		}
		us.mu.Unlock()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		us.endSessions(id, "")
		_ = os.RemoveAll(us.UserDir(u)) // their kubeconfigs go with them
		log.Printf("user %q deleted user %q", reqUser(r).Username, u.Username)
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
}

func keepIfSelf(r *http.Request, u *User) string {
	if reqUser(r).ID == u.ID {
		return reqSession(r)
	}
	return ""
}

// authenticate resolves the session for API requests. It returns the request with the
// user attached, or false after writing the error.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	p := r.URL.Path
	if p == "/api/auth/login" || p == "/api/auth/logout" {
		return r, true
	}
	c, err := r.Cookie(sessionCookie)
	var u *User
	if err == nil {
		u = s.users.sessionUser(c.Value)
	}
	if u == nil {
		writeJSON(w, 401, map[string]any{"error": "please sign in", "login": true})
		return r, false
	}
	// a temporary password must be replaced before anything else
	if u.MustChange && p != "/api/auth/me" && p != "/api/auth/password" {
		writeJSON(w, 403, map[string]any{"error": "change your password first", "mustChange": true})
		return r, false
	}
	return withUser(r, u, c.Value), true
}
