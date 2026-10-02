package sessiontest_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/julio641742/backendkit/session"
	"github.com/julio641742/backendkit/session/sessiontest"
)

type user struct {
	ID   int64
	Name string
}

func (u user) GetID() any { return u.ID }

var (
	alice = user{1, "alice"}
	bob   = user{2, "bob"}
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	store := sessiontest.NewStore[user]()
	now := time.Now()
	data := &session.Data[user]{ID: "k", User: &alice, ImpersonatedUser: &bob, CreatedOn: now, ExpiresOn: now.Add(time.Hour)}

	if err := store.Insert(ctx, data); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load(ctx, "k")
	if err != nil || *got.User != alice || *got.ImpersonatedUser != bob || !got.CreatedOn.Equal(now) {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	got.User.Name = "mallory"
	if again, _ := store.Load(ctx, "k"); again.User.Name != "alice" {
		t.Error("Load shares memory with the store")
	}

	if err := store.Touch(ctx, "k", now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "k"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Load of expired = %v, want ErrNotFound", err)
	}
	// Like pgstore's expires_on > now(): an expired session can't be revived.
	if err := store.Touch(ctx, "k", now.Add(time.Hour)); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Touch of expired = %v, want ErrNotFound", err)
	}

	if err := store.Destroy(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := store.Destroy(ctx, "k"); err != nil {
		t.Errorf("Destroy of a missing session = %v", err)
	}
	if err := store.Touch(ctx, "k", now); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Touch of a missing session = %v, want ErrNotFound", err)
	}
}

func TestStoreWithMiddleware(t *testing.T) {
	store := sessiontest.NewStore[user]()
	mw := session.Middleware(store)

	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := session.From[user](r)
		_ = s.SetUser(&alice)
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "https://example.com/login", nil))

	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	var got *user
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = session.From[user](r).GetUser()
	})).ServeHTTP(httptest.NewRecorder(), req)
	if got == nil || *got != alice {
		t.Errorf("user = %v, want alice", got)
	}
}

func TestStoreDuplicateInsert(t *testing.T) {
	ctx := context.Background()
	store := sessiontest.NewStore[user]()
	now := time.Now()
	data := &session.Data[user]{ID: "k", User: &alice, CreatedOn: now, ExpiresOn: now.Add(time.Hour)}
	if err := store.Insert(ctx, data); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, data); err == nil {
		t.Error("second insert of the same id succeeded")
	}
}

func TestStoreServiceSessions(t *testing.T) {
	ctx := context.Background()
	store := sessiontest.NewStore[user]()
	now := time.Now()
	if err := store.Insert(ctx, &session.Data[user]{ID: "parent", User: &alice, CreatedOn: now, ExpiresOn: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCode(ctx, "code", "parent", "a.svc", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCode(ctx, "old", "parent", "a.svc", now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	if err := store.RedeemCode(ctx, "old", "a.svc", "x"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("expired code redeemed: %v", err)
	}
	if err := store.RedeemCode(ctx, "code", "b.svc", "x"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("code redeemed at another host: %v", err)
	}
	if err := store.RedeemCode(ctx, "code", "a.svc", "svc"); err != nil {
		t.Fatal(err)
	}
	if err := store.RedeemCode(ctx, "code", "a.svc", "svc2"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("code redeemed twice: %v", err)
	}
	if d, err := store.LoadService(ctx, "svc", "a.svc"); err != nil || d.ID != "parent" || *d.User != alice {
		t.Errorf("LoadService = %+v, %v; want the parent session", d, err)
	}
	if _, err := store.LoadService(ctx, "svc", "b.svc"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("LoadService at another host = %v", err)
	}

	if err := store.Destroy(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadService(ctx, "svc", "a.svc"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("service session outlived its portal session: %v", err)
	}
}
