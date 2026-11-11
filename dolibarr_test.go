package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeDolibarr sert n adhérents « validés en cours » + quelques cas à écarter, sur plusieurs pages.
func fakeDolibarr(t *testing.T, key string) *httptest.Server {
	t.Helper()
	now := time.Now()
	future := now.Add(180 * 24 * time.Hour).Unix()
	past := now.Add(-24 * time.Hour).Unix()
	var all []string
	for i := 0; i < 130; i++ { // 130 valides => 2 pages
		all = append(all, fmt.Sprintf(`{"statut":"1","datefin":%d,"email":"m%d@ex.fr","lastname":"N%d","firstname":"P%d"}`, future, i, i, i))
	}
	all = append(all,
		fmt.Sprintf(`{"status":1,"datefin":"%d","email":"str@ex.fr","lastname":"Str","firstname":"Ing"}`, future), // formats chaîne / « status »
		`{"statut":"1","datefin":null,"email":"perpetuel@ex.fr","lastname":"Perp","firstname":"Etuel"}`,           // sans échéance
		fmt.Sprintf(`{"statut":"0","datefin":%d,"email":"brouillon@ex.fr","lastname":"B","firstname":"R"}`, future),
		fmt.Sprintf(`{"statut":"-1","datefin":%d,"email":"resilie@ex.fr","lastname":"R","firstname":"S"}`, future),
		fmt.Sprintf(`{"statut":"1","datefin":%d,"email":"expire@ex.fr","lastname":"E","firstname":"X"}`, past),
		fmt.Sprintf(`{"statut":"1","datefin":%d,"email":"","lastname":"Sans","firstname":"Mail"}`, future),
		fmt.Sprintf(`{"statut":"1","datefin":%d,"email":"pas un email","lastname":"Mauvais","firstname":"Mail"}`, future),
		fmt.Sprintf(`{"statut":"1","datefin":%d,"email":"M0@EX.FR","lastname":"Doublon","firstname":"M0"}`, future), // doublon (casse)
	)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("DOLAPIKEY") != key {
			http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/index.php/members" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var page, limit int
		fmt.Sscan(q.Get("page"), &page)
		fmt.Sscan(q.Get("limit"), &limit)
		start := page * limit
		if start >= len(all) {
			http.Error(w, `{"error":{"code":404,"message":"No member found"}}`, http.StatusNotFound)
			return
		}
		end := start + limit
		if end > len(all) {
			end = len(all)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "["+strings.Join(all[start:end], ",")+"]")
	}))
}

func TestDolibarrMembers(t *testing.T) {
	srv := fakeDolibarr(t, "secret")
	defer srv.Close()
	c := &DolibarrClient{BaseURL: srv.URL, Key: "secret"}
	res, err := c.Members(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// 130 + str + perpetuel = 132 à convoquer
	if len(res.Rows) != 132 {
		t.Fatalf("à convoquer = %d, attendu 132", len(res.Rows))
	}
	if res.Lus != 138 || res.Inactifs != 2 || res.Expires != 1 || res.SansEmail != 2 || res.Doublons != 1 {
		t.Fatalf("décomptes inattendus: lus=%d inactifs=%d expirés=%d sans email=%d doublons=%d", res.Lus, res.Inactifs, res.Expires, res.SansEmail, res.Doublons)
	}
	if res.Rows[0].Email != "m0@ex.fr" || res.Rows[0].Nom != "N0" {
		t.Fatalf("première ligne: %+v", res.Rows[0])
	}
}

func TestDolibarrBadKeyAndDown(t *testing.T) {
	srv := fakeDolibarr(t, "secret")
	c := &DolibarrClient{BaseURL: srv.URL, Key: "mauvaise"}
	_, err := c.Members(context.Background(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "refusé la clé") {
		t.Fatalf("clé refusée non signalée: %v", err)
	}
	if strings.Contains(err.Error(), "mauvaise") {
		t.Fatal("la clé apparaît dans l'erreur")
	}
	srv.Close()
	c = &DolibarrClient{BaseURL: srv.URL, Key: "secret"}
	if _, err := c.Members(context.Background(), time.Now()); err == nil {
		t.Fatal("serveur arrêté : erreur attendue")
	}
}

func TestSyncHandlers(t *testing.T) {
	dol := fakeDolibarr(t, "secret")
	defer dol.Close()
	st, _ := newTestStore(t, 2, time.Now().Add(time.Hour)) // 6 adhérents a..f@ex.fr déjà là
	loc, _ := time.LoadLocation("Europe/Paris")
	srv, err := NewServer(Config{BaseURL: "https://ag.hacf.fr", MaxMandats: 2, Deadline: st.Deadline, AGName: "AG", ContactEmail: "c@x.fr",
		AdminGroup: "Bureau", HdrUser: "X-Authentik-Username", HdrGroups: "X-Authentik-Groups",
		DolibarrURL: dol.URL, DolibarrKey: "secret", Loc: loc}, st)
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Routes()
	post := func(path string, admin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Origin", "https://ag.hacf.fr")
		if admin {
			req.Header.Set("X-Authentik-Username", "ludo")
			req.Header.Set("X-Authentik-Groups", "Bureau")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// sans droits d'admin : refusé
	if rec := post("/admin/sync/import", false); rec.Code != http.StatusForbidden {
		t.Fatalf("sync sans admin = %d", rec.Code)
	}
	before := len(st.Stats().Rows)

	// aperçu : ne modifie rien, annonce les chiffres
	rec := post("/admin/sync/preview", true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("aperçu = %d", rec.Code)
	}
	loc2, _ := url.Parse(rec.Header().Get("Location"))
	msg := loc2.Query().Get("msg")
	if !strings.Contains(msg, "132 à convoquer") || !strings.Contains(msg, "132 nouveau") {
		t.Fatalf("message d'aperçu: %q", msg)
	}
	if len(st.Stats().Rows) != before {
		t.Fatal("l'aperçu a modifié les données")
	}

	// import : CSV des nouveaux uniquement, 4 colonnes
	rec = post("/admin/sync/import", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("import = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 132 || len(strings.Split(lines[0], ";")) != 4 {
		t.Fatalf("CSV: %d lignes, première %q", len(lines), lines[0])
	}
	// second import : plus rien de nouveau
	rec = post("/admin/sync/import", true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("second import = %d", rec.Code)
	}
	if len(st.Stats().Rows) != before+132 {
		t.Fatalf("adhérents = %d", len(st.Stats().Rows))
	}
}

func TestSyncDisabledWithoutConfig(t *testing.T) {
	_, st, _, h := testServer(t)
	_ = st
	req := httptest.NewRequest("POST", "/admin/sync/import", nil)
	req.Header.Set("Origin", "https://ag.hacf.fr")
	req.Header.Set("X-Authentik-Username", "ludo")
	req.Header.Set("X-Authentik-Groups", "hacf-ag-admin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "non+configur") {
		t.Fatalf("sans config: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}
