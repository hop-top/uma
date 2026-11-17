package imap_test

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	"github.com/xdg-go/scram"

	"hop.top/uma/adapter/mail/imap"
)

// security is how a test server listens.
type security int

const (
	implicitTLS security = iota // TLS from the first byte
	startTLS                    // plaintext, STARTTLS offered, AUTH only after it
	plaintext                   // no TLS at all; AUTH allowed anyway
)

// serverOptions shape one test IMAP server.
type serverOptions struct {
	sec security
	// mechs are the SASL mechanisms advertised; nil is PLAIN.
	mechs []string
	// noIdle strips IDLE from every capability list.
	noIdle bool
	// tokenOK decides bearer tokens; nil accepts none.
	tokenOK func(string) bool
	// anyUser accepts bearer tokens for any user name.
	anyUser bool
	// afterSearch and afterSelect, when set, run after each SEARCH or
	// SELECT/EXAMINE is answered from the mailbox, before the reply is
	// written.
	afterSearch, afterSelect func()
	// afterSelectBox runs after each SELECT or EXAMINE, with the mailbox.
	afterSelectBox func(mailbox string)
	// state advertises UIDPLUS, MOVE and SPECIAL-USE, as a server that
	// moves mail does; noMove leaves MOVE out of it.
	state, noMove bool
	// delim, when set, is the hierarchy delimiter the server shows and takes
	// in place of the memory server's "/": mailboxes are listed and created
	// under dotted names (see session.List and session.Create).
	delim rune
	// noCreate makes CREATE answer NO [CANNOT].
	noCreate bool
}

// login is one successful authentication the server saw.
type login struct {
	mech  string
	token string
	tls   bool
	at    time.Time
}

// mailServer is an in-process IMAP server over imapmemserver with one user,
// mailUser, whose INBOX exists. It records authentications, client command
// lines (on implicit TLS and plaintext listeners) and open connections.
type mailServer struct {
	t    *testing.T
	opts serverOptions
	port int
	user *imapmemserver.User

	mu       sync.Mutex
	fetch    func(goimap.NumSet, *goimap.FetchOptions) bool
	cut      string
	boxes    []box
	script   []stateReply
	scripted lastScripted
	logins   []login
	attempts []string
	lines    []string
	conns    []net.Conn
}

func newMailServer(t *testing.T, opts serverOptions) *mailServer {
	t.Helper()
	if opts.mechs == nil {
		opts.mechs = []string{sasl.Plain}
	}
	s := &mailServer{t: t, opts: opts, user: imapmemserver.NewUser(mailUser, mailPass)}
	if err := s.user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem := imapmemserver.New()
	mem.AddUser(s.user)
	s.boxes = []box{{name: "INBOX"}}
	caps := goimap.CapSet{goimap.CapIMAP4rev1: {}}
	if opts.state {
		caps[goimap.CapUIDPlus], caps[goimap.CapSpecialUse] = struct{}{}, struct{}{}
		if !opts.noMove {
			caps[goimap.CapMove] = struct{}{}
		}
	}
	so := &imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{Session: mem.NewSession(), srv: s, conn: c}, nil, nil
		},
		Caps:   caps,
		Logger: discardLog{},
	}
	ln := listen(t)
	s.port = port(ln)
	switch opts.sec {
	case implicitTLS:
		so.InsecureAuth = true // the tap hides *tls.Conn; the client enforces TLS
		ln = &tapListener{Listener: tls.NewListener(ln, serverTLS(t)), s: s}
	case startTLS:
		so.TLSConfig = serverTLS(t)
		ln = &tapListener{Listener: ln, s: s, blind: true}
	case plaintext:
		so.InsecureAuth = true
		ln = &tapListener{Listener: ln, s: s}
	}
	srv := imapserver.New(so)
	served := make(chan struct{})
	go func() { defer close(served); _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		// Serve must have returned before Close: imapserver adds to a
		// WaitGroup in Serve that Close waits on, unsynchronized.
		_ = ln.Close()
		<-served
		_ = srv.Close()
	})
	return s
}

type discardLog struct{}

func (discardLog) Printf(string, ...any) {}
func (discardLog) Println(...any)        {}

// endpoint is the server as a channel configuration.
func (s *mailServer) endpoint() imap.Server {
	e := imap.Server{Host: "127.0.0.1", CommandTimeoutMS: 5000}
	switch s.opts.sec {
	case implicitTLS:
		e.TLSPort = s.port
	case startTLS:
		e.StartTLSPort = s.port
	case plaintext:
		e.PlainPort, e.AllowPlaintextLoopback = s.port, true
	}
	return e
}

// deliver appends raw to mailbox with an INTERNALDATE.
func (s *mailServer) deliver(mailbox string, raw []byte, at time.Time) {
	s.t.Helper()
	if _, err := s.user.Append(mailbox, literal{bytes.NewReader(raw), int64(len(raw))}, &goimap.AppendOptions{Time: at}); err != nil {
		s.t.Fatalf("append to %s: %v", mailbox, err)
	}
}

// expunge removes the messages with the given UIDs from mailbox, as another
// session of the account would: \Deleted, then EXPUNGE. Sessions that have
// the mailbox open learn of it with their next command.
func (s *mailServer) expunge(mailbox string, uids ...uint32) {
	s.t.Helper()
	sess := imapmemserver.NewUserSession(s.user)
	defer func() { _ = sess.Close() }()
	if _, err := sess.Select(mailbox, nil); err != nil {
		s.t.Fatal(err)
	}
	var set goimap.UIDSet
	for _, u := range uids {
		set.AddNum(goimap.UID(u))
	}
	flags := &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Silent: true, Flags: []goimap.Flag{goimap.FlagDeleted}}
	if err := sess.Store(nil, set, flags, nil); err != nil {
		s.t.Fatal(err)
	}
	if err := sess.Expunge(nil, &set); err != nil {
		s.t.Fatal(err)
	}
}

// onFetch sets f to run before each FETCH is answered from the mailbox; nil
// clears it. f runs on the server's goroutine and may change the mailbox. A
// true result answers the FETCH with OK and no data, whatever the mailbox
// holds.
func (s *mailServer) onFetch(f func(goimap.NumSet, *goimap.FetchOptions) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetch = f
}

// cutAt makes the server close the connection in the middle of the first
// write that holds marker, right after the marker.
func (s *mailServer) cutAt(marker string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cut = marker
}

// takeCut returns the offset in b to cut the connection at, or -1.
func (s *mailServer) takeCut(b []byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cut == "" {
		return -1
	}
	i := bytes.Index(b, []byte(s.cut))
	if i < 0 {
		return -1
	}
	n := i + len(s.cut)
	s.cut = ""
	return n
}

// recreate deletes and recreates mailbox: a new UIDVALIDITY, UIDs restart.
func (s *mailServer) recreate(mailbox string) {
	s.t.Helper()
	if err := s.user.Delete(mailbox); err != nil {
		s.t.Fatal(err)
	}
	if err := s.user.Create(mailbox, nil); err != nil {
		s.t.Fatal(err)
	}
}

// uidValidity returns mailbox's UIDVALIDITY.
func (s *mailServer) uidValidity(mailbox string) uint32 {
	s.t.Helper()
	st, err := s.user.Status(mailbox, &goimap.StatusOptions{UIDValidity: true})
	if err != nil {
		s.t.Fatal(err)
	}
	return st.UIDValidity
}

// seen reports how many messages of mailbox carry \Seen.
func (s *mailServer) seen(mailbox string) uint32 {
	s.t.Helper()
	st, err := s.user.Status(mailbox, &goimap.StatusOptions{NumUnseen: true, NumMessages: true})
	if err != nil {
		s.t.Fatal(err)
	}
	return *st.NumMessages - *st.NumUnseen
}

func (s *mailServer) count(mailbox string) uint32 {
	s.t.Helper()
	st, err := s.user.Status(mailbox, &goimap.StatusOptions{NumMessages: true})
	if err != nil {
		s.t.Fatal(err)
	}
	return *st.NumMessages
}

func (s *mailServer) loginList() []login {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]login(nil), s.logins...)
}

func (s *mailServer) attemptList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.attempts...)
}

// clientLines returns the command lines clients sent, on listeners the tap
// can read.
func (s *mailServer) clientLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// commands counts client lines whose command (after the tag) is name.
func (s *mailServer) commands(name string) int {
	n := 0
	for _, l := range s.clientLines() {
		if f := strings.Fields(l); len(f) >= 2 && strings.EqualFold(f[1], name) {
			n++
		}
	}
	return n
}

// drop closes every open connection.
func (s *mailServer) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

func (s *mailServer) record(l login) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logins = append(s.logins, l)
}

type literal struct {
	io.Reader
	n int64
}

func (l literal) Size() int64 { return l.n }

// session is the memory session with SASL mechanisms and accounting.
type session struct {
	imapserver.Session
	srv  *mailServer
	conn *imapserver.Conn
}

func (s *session) AuthenticateMechanisms() []string { return s.srv.opts.mechs }

func (s *session) tlsState() *tls.ConnectionState {
	if c, ok := s.conn.NetConn().(interface{ ConnectionState() tls.ConnectionState }); ok {
		if cs := c.ConnectionState(); cs.HandshakeComplete {
			return &cs
		}
	}
	return nil
}

func (s *session) Authenticate(mech string) (sasl.Server, error) {
	s.srv.mu.Lock()
	s.srv.attempts = append(s.srv.attempts, mech)
	s.srv.mu.Unlock()
	state := s.tlsState()
	done := func(token string) error {
		if err := s.Session.Login(mailUser, mailPass); err != nil {
			return err
		}
		s.srv.record(login{mech: mech, token: token, tls: state != nil, at: time.Now()})
		return nil
	}
	tokenOK := func(tok string) bool { return s.srv.opts.tokenOK != nil && s.srv.opts.tokenOK(tok) }
	userOK := func(u string) bool { return u == mailUser || s.srv.opts.anyUser }
	switch strings.ToUpper(mech) {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, user, pass string) error {
			if (identity == "" || identity == user) && user == mailUser && pass == mailPass {
				return done("")
			}
			return imapserver.ErrAuthFailed
		}), nil
	case sasl.Login:
		return &loginServer{done: done}, nil
	case "XOAUTH2":
		return &xoauth2Server{ok: tokenOK, userOK: userOK, done: done}, nil
	case sasl.OAuthBearer:
		return sasl.NewOAuthBearerServer(func(o sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
			if userOK(o.Username) && tokenOK(o.Token) && done(o.Token) == nil {
				return nil
			}
			return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
		}), nil
	case sasl.External:
		return sasl.NewExternalServer(func(identity string) error {
			if state == nil || len(state.PeerCertificates) == 0 || state.PeerCertificates[0].Subject.CommonName != mailUser ||
				identity != "" && identity != mailUser {
				return imapserver.ErrAuthFailed
			}
			return done("")
		}), nil
	case "SCRAM-SHA-256", "SCRAM-SHA-256-PLUS":
		return newSCRAMServer(strings.ToUpper(mech), state, done)
	}
	return nil, imapserver.ErrAuthFailed
}

// List answers from the mailboxes made with create when any has a special
// use, which the memory server cannot hold; otherwise from the memory server.
func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *goimap.ListOptions) error {
	boxes := s.srv.boxList()
	if d := s.srv.opts.delim; d != 0 {
		if len(patterns) == 0 || (len(patterns) == 1 && patterns[0] == "") {
			return w.WriteList(&goimap.ListData{Attrs: []goimap.MailboxAttr{goimap.MailboxAttrNoSelect}, Delim: d})
		}
		for _, b := range boxes {
			match := false
			for _, p := range patterns {
				match = match || imapserver.MatchList(b.name, d, ref, p)
			}
			if !match {
				continue
			}
			data := &goimap.ListData{Mailbox: b.name, Delim: d}
			if options.ReturnSpecialUse || options.SelectSpecialUse {
				data.Attrs = b.attrs
			}
			if err := w.WriteList(data); err != nil {
				return err
			}
		}
		return nil
	}
	special := false
	for _, b := range boxes {
		special = special || len(b.attrs) > 0
	}
	if !special || len(patterns) == 0 {
		return s.Session.List(w, ref, patterns, options)
	}
	for _, b := range boxes {
		match := false
		for _, p := range patterns {
			match = match || imapserver.MatchList(b.name, '/', ref, p)
		}
		if !match {
			continue
		}
		data := &goimap.ListData{Mailbox: b.name, Delim: '/'}
		if options.ReturnSpecialUse || options.SelectSpecialUse {
			data.Attrs = b.attrs
		}
		if options.SelectSpecialUse && len(b.attrs) == 0 {
			continue
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

// Create is CREATE: refused when the server is set to, else recorded, and
// under a delimiter other than "/" kept as the client names it.
func (s *session) Create(name string, options *goimap.CreateOptions) error {
	if s.srv.opts.noCreate {
		return &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeCannot, Text: "no"}
	}
	d := s.srv.opts.delim
	if d == 0 {
		if err := s.Session.Create(name, options); err != nil {
			return err
		}
		s.srv.mu.Lock()
		s.srv.boxes = append(s.srv.boxes, box{name: name})
		s.srv.mu.Unlock()
		return nil
	}
	for _, b := range s.srv.boxList() {
		if b.name == name {
			return &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeAlreadyExists, Text: "exists"}
		}
	}
	if err := s.Session.Create(strings.ReplaceAll(name, string(d), "/"), options); err != nil {
		return err
	}
	s.srv.mu.Lock()
	s.srv.boxes = append(s.srv.boxes, box{name: name})
	s.srv.mu.Unlock()
	return nil
}

// Move is the memory server's MOVE, under the state script.
func (s *session) Move(w *imapserver.MoveWriter, numSet goimap.NumSet, dest string) error {
	apply, after, err := s.srv.next(s.conn)
	if err != nil || !apply {
		return err
	}
	defer after()
	return s.Session.(imapserver.SessionMove).Move(w, numSet, dest)
}

// Copy is the memory server's COPY, under the state script.
func (s *session) Copy(numSet goimap.NumSet, dest string) (*goimap.CopyData, error) {
	apply, after, err := s.srv.next(s.conn)
	if err != nil || !apply {
		return nil, err
	}
	defer after()
	return s.Session.Copy(numSet, dest)
}

// Store is the memory server's STORE, under the state script when it sets
// a flag the state verbs own.
func (s *session) Store(w *imapserver.FetchWriter, numSet goimap.NumSet, flags *goimap.StoreFlags, options *goimap.StoreOptions) error {
	if len(flags.Flags) == 1 && flags.Flags[0] == goimap.FlagDeleted {
		return s.Session.Store(w, numSet, flags, options)
	}
	apply, after, err := s.srv.next(s.conn)
	if err != nil || !apply {
		return err
	}
	defer after()
	return s.Session.Store(w, numSet, flags, options)
}

func (s *session) Search(kind imapserver.NumKind, criteria *goimap.SearchCriteria, options *goimap.SearchOptions) (*goimap.SearchData, error) {
	data, err := s.Session.Search(kind, criteria, options)
	if f := s.srv.opts.afterSearch; f != nil {
		f()
	}
	return data, err
}

func (s *session) Fetch(w *imapserver.FetchWriter, numSet goimap.NumSet, options *goimap.FetchOptions) error {
	s.srv.mu.Lock()
	f := s.srv.fetch
	s.srv.mu.Unlock()
	if f != nil && f(numSet, options) {
		return nil
	}
	return s.Session.Fetch(w, numSet, options)
}

func (s *session) Select(mailbox string, options *goimap.SelectOptions) (*goimap.SelectData, error) {
	data, err := s.Session.Select(mailbox, options)
	if f := s.srv.opts.afterSelect; f != nil && err == nil {
		f()
	}
	if f := s.srv.opts.afterSelectBox; f != nil && err == nil {
		f(mailbox)
	}
	return data, err
}

// Idle reports mailbox changes to an idling client as they are queued,
// including those queued just before IDLE started: the memory server only
// reports changes made while its IDLE wait is registered, real servers
// report pending ones too.
func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	t := time.NewTicker(2 * time.Millisecond)
	defer t.Stop()
	for {
		if err := s.Poll(w, true); err != nil {
			return err
		}
		select {
		case <-stop:
			return nil
		case <-t.C:
		}
	}
}

// Login is the LOGIN command.
func (s *session) Login(user, pass string) error {
	s.srv.mu.Lock()
	s.srv.attempts = append(s.srv.attempts, "IMAP-LOGIN")
	s.srv.mu.Unlock()
	if err := s.Session.Login(user, pass); err != nil {
		return err
	}
	s.srv.record(login{mech: "IMAP-LOGIN", tls: s.tlsState() != nil, at: time.Now()})
	return nil
}

type loginServer struct {
	done func(string) error
	step int
	user string
}

func (s *loginServer) Next(resp []byte) ([]byte, bool, error) {
	s.step++
	switch s.step {
	case 1:
		return []byte("Username:"), false, nil
	case 2:
		s.user = string(resp)
		return []byte("Password:"), false, nil
	}
	if s.user == mailUser && string(resp) == mailPass {
		return nil, true, s.done("")
	}
	return nil, true, imapserver.ErrAuthFailed
}

// xoauth2Server follows Google: a bad token gets a JSON error challenge, then
// fails after the client's empty response.
type xoauth2Server struct {
	ok, userOK func(string) bool
	done       func(string) error
	failed     bool
}

func (s *xoauth2Server) Next(resp []byte) ([]byte, bool, error) {
	if s.failed {
		return nil, true, imapserver.ErrAuthFailed
	}
	if resp == nil {
		return []byte{}, false, nil
	}
	var user, tok string
	for _, kv := range bytes.Split(resp, []byte{0x01}) {
		k, v, _ := strings.Cut(string(kv), "=")
		switch k {
		case "user":
			user = v
		case "auth":
			tok = strings.TrimPrefix(v, "Bearer ")
		}
	}
	if s.userOK(user) && s.ok(tok) {
		return nil, true, s.done(tok)
	}
	s.failed = true
	return []byte(`{"status":"401","schemes":"bearer"}`), false, nil
}

// scramServer sends the server-final message as a challenge, as Dovecot
// does; the client answers it with an empty response.
type scramServer struct {
	conv *scram.ServerConversation
	done func(string) error
	sent bool
}

func newSCRAMServer(mech string, state *tls.ConnectionState, done func(string) error) (sasl.Server, error) {
	client, err := scram.SHA256.NewClient(mailUser, mailPass, "")
	if err != nil {
		return nil, err
	}
	stored, err := client.GetStoredCredentialsWithError(scram.KeyFactors{Salt: "c2FsdHNhbHQ=", Iters: 4096})
	if err != nil {
		return nil, err
	}
	srv, err := scram.SHA256.NewServer(func(user string) (scram.StoredCredentials, error) {
		if user != mailUser {
			return scram.StoredCredentials{}, errors.New("unknown user")
		}
		return stored, nil
	})
	if err != nil {
		return nil, err
	}
	conv := srv.NewConversation()
	if mech == "SCRAM-SHA-256-PLUS" {
		if state == nil {
			return nil, imapserver.ErrAuthFailed
		}
		cb, err := scram.NewTLSExporterBinding(state)
		if err != nil {
			return nil, err
		}
		conv = srv.NewConversationWithChannelBindingRequired(cb)
	}
	return &scramServer{conv: conv, done: done}, nil
}

func (s *scramServer) Next(resp []byte) ([]byte, bool, error) {
	if s.sent {
		return nil, true, s.done("")
	}
	if resp == nil {
		return []byte{}, false, nil
	}
	out, err := s.conv.Step(string(resp))
	if err != nil {
		return nil, true, imapserver.ErrAuthFailed
	}
	if s.conv.Done() {
		if !s.conv.Valid() {
			return nil, true, imapserver.ErrAuthFailed
		}
		s.sent = true
	}
	return []byte(out), false, nil
}

// tapListener records client lines, tracks connections and strips IDLE
// from capability lists when asked. blind taps sit below a STARTTLS upgrade
// and record nothing.
type tapListener struct {
	net.Listener
	s     *mailServer
	blind bool
}

func (l *tapListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.s.mu.Lock()
	l.s.conns = append(l.s.conns, c)
	l.s.mu.Unlock()
	if l.blind {
		return c, nil
	}
	return &tapConn{Conn: c, s: l.s}, nil
}

type tapConn struct {
	net.Conn
	s   *mailServer
	buf []byte
}

// ConnectionState exposes the TLS state of an implicit TLS connection.
func (c *tapConn) ConnectionState() tls.ConnectionState {
	if tc, ok := c.Conn.(*tls.Conn); ok {
		return tc.ConnectionState()
	}
	return tls.ConnectionState{}
}

func (c *tapConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.buf = append(c.buf, b[:n]...)
		for {
			i := bytes.Index(c.buf, []byte("\r\n"))
			if i < 0 {
				break
			}
			c.s.mu.Lock()
			c.s.lines = append(c.s.lines, string(c.buf[:i]))
			c.s.mu.Unlock()
			c.buf = c.buf[i+2:]
		}
	}
	return n, err
}

func (c *tapConn) Write(b []byte) (int, error) {
	if n := c.s.takeCut(b); n >= 0 {
		_, _ = c.Conn.Write(b[:n])
		_ = c.Close()
		return n, io.ErrClosedPipe
	}
	if !c.s.opts.noIdle || !bytes.Contains(b, []byte(" IDLE")) {
		return c.Conn.Write(b)
	}
	if _, err := c.Conn.Write(bytes.ReplaceAll(b, []byte(" IDLE"), nil)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// box is a mailbox made with create and its special-use attributes.
type box struct {
	name  string
	attrs []goimap.MailboxAttr
}

// create makes mailbox with the given special-use attributes.
func (s *mailServer) create(name string, attrs ...goimap.MailboxAttr) {
	s.t.Helper()
	if err := s.user.Create(name, nil); err != nil {
		s.t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boxes = append(s.boxes, box{name: name, attrs: attrs})
}

func (s *mailServer) boxList() []box {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]box(nil), s.boxes...)
	slices.SortFunc(out, func(a, b box) int { return strings.Compare(a.name, b.name) })
	return out
}

// attrsOf returns the special-use attributes of mailbox.
func (s *mailServer) attrsOf(name string) []goimap.MailboxAttr {
	for _, b := range s.boxList() {
		if b.name == name {
			return b.attrs
		}
	}
	return nil
}

// stateReply scripts what the server does with the next state command
// (STORE of a flag, MOVE or COPY): apply or not, and whether the connection
// is cut before the reply, as a lost request or a lost answer.
type stateReply struct {
	refuse    *goimap.Error
	apply     bool
	cut       bool
	lostReply bool // applied, answer lost
}

// lastScripted remembers the last scripted reply that was consumed.
type lastScripted struct {
	set       bool
	lostReply bool
}

// respond queues replies for the next state commands.
func (s *mailServer) respond(rs ...stateReply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = append(s.script, rs...)
}

// takeScripted returns the last scripted reply consumed since the last call.
func (s *mailServer) takeScripted() lastScripted {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.scripted
	s.scripted = lastScripted{}
	return l
}

// next consumes the script for a state command: apply reports whether the
// command runs, and after must run once it did. A refusal comes back as the
// error; a lost request closes the connection without running the command, a
// lost answer closes it once the command ran.
func (s *mailServer) next(c *imapserver.Conn) (apply bool, after func(), err error) {
	s.mu.Lock()
	if len(s.script) == 0 {
		s.mu.Unlock()
		return true, func() {}, nil
	}
	r := s.script[0]
	s.script = s.script[1:]
	s.scripted = lastScripted{set: true, lostReply: r.lostReply}
	s.mu.Unlock()
	switch {
	case r.refuse != nil:
		return false, nil, r.refuse
	case r.lostReply:
		return true, func() { _ = c.NetConn().Close() }, nil
	case r.cut:
		_ = c.NetConn().Close()
		return false, nil, io.ErrClosedPipe
	}
	return r.apply, func() {}, nil
}
