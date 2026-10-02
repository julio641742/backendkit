package pgstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/julio641742/backendkit/session"
)

var (
	alice = testUser{ID: 1, Email: "alice@example.com"}
	bob   = testUser{ID: 2, Email: "bob@example.com"}
)

func newStore(db *fakeDB) *Store[testUser] {
	return New[testUser](db, getUserQuery)
}

// insert stores a new session for user through the store.
func insert(t *testing.T, store *Store[testUser], id string, user testUser, impersonated *testUser) *session.Data[testUser] {
	t.Helper()
	now := time.Now()
	data := &session.Data[testUser]{ID: id, User: &user, ImpersonatedUser: impersonated, CreatedOn: now, ExpiresOn: now.Add(time.Hour)}
	if err := store.Insert(context.Background(), data); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return data
}

func TestInsertAndLoad(t *testing.T) {
	db := newFakeDB(alice, bob)
	store := newStore(db)
	saved := insert(t, store, "k", alice, nil)

	row, ok := db.session("k")
	if !ok || row.userID != alice.ID || row.impersonatedUserID != nil {
		t.Fatalf("row = %+v, %v", row, ok)
	}

	got, err := store.Load(context.Background(), "k")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != "k" || got.User == nil || *got.User != alice || got.ImpersonatedUser != nil {
		t.Errorf("Load = %+v", got)
	}
	if !got.CreatedOn.Equal(saved.CreatedOn) || !got.ExpiresOn.Equal(saved.ExpiresOn) {
		t.Errorf("times = %v %v, want %v %v", got.CreatedOn, got.ExpiresOn, saved.CreatedOn, saved.ExpiresOn)
	}

	insert(t, store, "imp", alice, &bob)
	if row, _ := db.session("imp"); row.impersonatedUserID != bob.ID {
		t.Errorf("impersonated_user_id = %v, want %v", row.impersonatedUserID, bob.ID)
	}
	got, err = store.Load(context.Background(), "imp")
	if err != nil || *got.User != alice || got.ImpersonatedUser == nil || *got.ImpersonatedUser != bob {
		t.Errorf("Load = %+v, %v", got, err)
	}
}

func TestTouchAndDestroy(t *testing.T) {
	db := newFakeDB(alice)
	store := newStore(db)
	insert(t, store, "k", alice, nil)

	expiresOn := time.Now().Add(5 * time.Hour)
	if err := store.Touch(context.Background(), "k", expiresOn); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if row, _ := db.session("k"); !row.expiresOn.Equal(expiresOn) {
		t.Errorf("expires_on = %v, want %v", row.expiresOn, expiresOn)
	}

	if err := store.Destroy(context.Background(), "k"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, ok := db.session("k"); ok {
		t.Error("session not deleted")
	}
	if err := store.Destroy(context.Background(), "k"); err != nil {
		t.Errorf("Destroy of a missing session = %v", err)
	}
}

func TestTouchMissingSession(t *testing.T) {
	store := newStore(newFakeDB(alice))
	if err := store.Touch(context.Background(), "gone", time.Now().Add(time.Hour)); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Touch = %v, want ErrNotFound", err)
	}
}

// An expired row that wasn't purged yet counts as missing, so it can't be
// revived by a renewal.
func TestTouchExpiredSession(t *testing.T) {
	db := newFakeDB(alice)
	store := newStore(db)
	insert(t, store, "k", alice, nil)
	row, _ := db.session("k")
	row.expiresOn = time.Now().Add(-time.Minute)

	if err := store.Touch(context.Background(), "k", time.Now().Add(time.Hour)); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Touch = %v, want ErrNotFound", err)
	}
	if row.expiresOn.After(time.Now()) {
		t.Error("the expired session was revived")
	}
	for _, q := range []string{loadQuery, touchQuery} {
		if !strings.Contains(q, "expires_on > now()") {
			t.Errorf("query doesn't skip expired rows: %q", q)
		}
	}
}

func TestLoadNotFound(t *testing.T) {
	tests := []struct {
		name  string
		setup func(db *fakeDB)
	}{
		{"missing", func(db *fakeDB) { delete(db.sessions, "k") }},
		{"expired", func(db *fakeDB) { db.sessions["k"].expiresOn = time.Now().Add(-time.Minute) }},
		{"user deleted", func(db *fakeDB) { delete(db.users, alice.ID) }},
		{"impersonated user deleted", func(db *fakeDB) {
			db.sessions["k"].impersonatedUserID = bob.ID
			delete(db.users, bob.ID)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newFakeDB(alice, bob)
			store := newStore(db)
			insert(t, store, "k", alice, nil)
			tt.setup(db)

			if _, err := store.Load(context.Background(), "k"); !errors.Is(err, session.ErrNotFound) {
				t.Errorf("Load = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestLoadError(t *testing.T) {
	db := newFakeDB(alice)
	store := newStore(db)
	insert(t, store, "k", alice, nil)
	boom := errors.New("connection refused")
	db.queryErr = boom

	if _, err := store.Load(context.Background(), "k"); !errors.Is(err, boom) || errors.Is(err, session.ErrNotFound) {
		t.Errorf("Load = %v, want %v", err, boom)
	}
}

func TestPurgeExpired(t *testing.T) {
	db := newFakeDB(alice)
	db.sessions["old"] = &fakeSession{userID: alice.ID, expiresOn: time.Now().Add(-time.Hour)}
	store := newStore(db)
	insert(t, store, "new", alice, nil)

	if err := store.PurgeExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.session("old"); ok {
		t.Error("expired session not purged")
	}
	if _, ok := db.session("new"); !ok {
		t.Error("new session was purged")
	}
}

func TestServiceSessions(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB(alice)
	store := newStore(db)
	insert(t, store, "parent", alice, nil)

	const host = "a.svc.example.net"
	if err := store.InsertCode(ctx, "code", "parent", host, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadService(ctx, "code", host); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("LoadService on an unredeemed code = %v, want ErrNotFound", err)
	}
	if err := store.RedeemCode(ctx, "code", "b.svc.example.net", "svc"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("RedeemCode at another host = %v, want ErrNotFound", err)
	}
	if err := store.RedeemCode(ctx, "code", host, "svc"); err != nil {
		t.Fatalf("RedeemCode = %v", err)
	}
	if err := store.RedeemCode(ctx, "code", host, "svc2"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("second RedeemCode = %v, want ErrNotFound", err)
	}
	if d, err := store.LoadService(ctx, "svc", host); err != nil || d.ID != "parent" || *d.User != alice {
		t.Errorf("LoadService = %+v, %v; want the parent session", d, err)
	}
	if _, err := store.LoadService(ctx, "svc", "b.svc.example.net"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("LoadService at another host = %v, want ErrNotFound", err)
	}

	if err := store.Destroy(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadService(ctx, "svc", host); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("service session outlived its portal session: %v", err)
	}
}

func TestExpiredCodes(t *testing.T) {
	ctx := context.Background()
	db := newFakeDB(alice)
	store := newStore(db)
	insert(t, store, "parent", alice, nil)
	const host = "a.svc.example.net"
	if err := store.InsertCode(ctx, "code", "parent", host, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.RedeemCode(ctx, "code", host, "svc"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("RedeemCode on an expired code = %v, want ErrNotFound", err)
	}
	if err := store.PurgeExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if len(db.services) != 0 {
		t.Error("expired code not purged")
	}
}

func TestMissingGetUserPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	New[testUser](newFakeDB(), "")
}

// TestWithMiddleware wires the store into session.Middleware: a login is
// stored under the hashed id and read back on the next request.
func TestWithMiddleware(t *testing.T) {
	db := newFakeDB(alice)
	mw := session.Middleware(newStore(db))

	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := session.From[testUser](r)
		if err := s.SetUser(&alice); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "https://example.com/login", nil))

	if n := db.sessionCount(); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	for _, c := range rec.Result().Cookies() {
		if _, ok := db.session(c.Value); ok {
			t.Error("raw cookie value used as the stored id")
		}
		req.AddCookie(c)
	}
	var user *testUser
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user = session.From[testUser](r).GetUser()
	})).ServeHTTP(httptest.NewRecorder(), req)
	if user == nil || *user != alice {
		t.Errorf("user = %v, want alice", user)
	}
}
