package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, max int, deadline time.Time) (*Store, map[string]IssuedToken) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "state.json"), max, deadline)
	if err != nil {
		t.Fatal(err)
	}
	var rows []ImportRow
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		rows = append(rows, ImportRow{Email: n + "@ex.fr", Nom: strings.ToUpper(n), Prenom: "P" + n})
	}
	issued, _, err := st.ImportMembers(rows, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]IssuedToken{}
	for _, i := range issued {
		m[i.Email[:1]] = i
	}
	return st, m
}

func id(t *testing.T, st *Store, tok IssuedToken) int {
	t.Helper()
	m, ok := st.MemberByToken(tok.Token)
	if !ok {
		t.Fatal("token inconnu")
	}
	return m.ID
}

func TestTokenLookup(t *testing.T) {
	st, toks := newTestStore(t, 3, time.Now().Add(time.Hour))
	if _, ok := st.MemberByToken(toks["a"].Token); !ok {
		t.Fatal("token valide refusé")
	}
	if _, ok := st.MemberByToken(toks["a"].Token + "x"); ok {
		t.Fatal("token altéré accepté")
	}
	if _, ok := st.MemberByToken(""); ok {
		t.Fatal("token vide accepté")
	}
	// Le token en clair n'est jamais stocké.
	for _, m := range st.st.Members {
		if m.TokenHash == toks[m.Email[:1]].Token {
			t.Fatal("token stocké en clair")
		}
	}
}

func TestMandateRules(t *testing.T) {
	st, toks := newTestStore(t, 2, time.Now().Add(time.Hour))
	a, b, c, d, e := id(t, st, toks["a"]), id(t, st, toks["b"]), id(t, st, toks["c"]), id(t, st, toks["d"]), id(t, st, toks["e"])

	if err := st.Save(a, ChoixMandat, a, "a"); err == nil {
		t.Fatal("procuration à soi-même acceptée")
	}
	if err := st.Save(a, ChoixMandat, 9999, "a"); err == nil {
		t.Fatal("mandataire inconnu accepté")
	}
	// un mandataire qui n'a pas répondu est refusé, et absent de la liste proposée
	if err := st.Save(a, ChoixMandat, e, "a"); err == nil {
		t.Fatal("mandataire sans réponse accepté")
	}
	if err := st.Save(c, ChoixMandat, d, "c"); err == nil {
		t.Fatal("mandataire sans réponse accepté (d)")
	}
	for _, m := range st.EligibleMandataires(a) {
		t.Fatalf("aucun adhérent n'est présent, mais %d est proposé", m.ID)
	}
	if err := st.Save(e, ChoixPresent, 0, "e"); err != nil {
		t.Fatal(err)
	}
	if got := st.EligibleMandataires(a); len(got) != 1 || got[0].ID != e {
		t.Fatalf("seul e (présent) devrait être proposé: %v", got)
	}
	if err := st.Save(a, ChoixMandat, e, "a"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(b, ChoixMandat, e, "b"); err != nil {
		t.Fatal(err)
	}
	// plafond (2) atteint
	if err := st.Save(c, ChoixMandat, e, "c"); err == nil {
		t.Fatal("plafond dépassé accepté")
	}
	// modifier sa propre procuration vers le même mandataire ne compte pas deux fois
	if err := st.Save(a, ChoixMandat, e, "a"); err != nil {
		t.Fatalf("re-sauvegarde refusée: %v", err)
	}
	// e détient 2 procurations : ne peut être absent ni donner la sienne
	if err := st.Save(e, ChoixAbsent, 0, "e"); err == nil {
		t.Fatal("détenteur de procuration absent accepté")
	}
	if err := st.Save(e, ChoixMandat, d, "e"); err == nil {
		t.Fatal("détenteur de procuration a pu donner la sienne")
	}
	if err := st.Save(e, ChoixPresent, 0, "e"); err != nil {
		t.Fatal(err)
	}
	// un adhérent absent ne peut pas être mandataire
	if err := st.Save(d, ChoixAbsent, 0, "d"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(c, ChoixMandat, d, "c"); err == nil {
		t.Fatal("mandataire absent accepté")
	}
	// changer d'avis libère la place
	if err := st.Save(b, ChoixPresent, 0, "b"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(c, ChoixMandat, e, "c"); err != nil {
		t.Fatalf("place libérée mais refus: %v", err)
	}
	if err := st.Save(a, "nimporte", 0, "a"); err == nil {
		t.Fatal("choix invalide accepté")
	}
	// éligibles : e est complet (a, c), donc absent de la liste de b
	for _, m := range st.EligibleMandataires(b) {
		if m.ID == e {
			t.Fatal("mandataire complet proposé")
		}
		if m.ID == b {
			t.Fatal("soi-même proposé")
		}
		if m.Email != "" {
			t.Fatal("email exposé dans la liste des mandataires")
		}
	}
}

func TestDeadline(t *testing.T) {
	st, toks := newTestStore(t, 3, time.Now().Add(time.Hour))
	a := id(t, st, toks["a"])
	if err := st.Save(a, ChoixPresent, 0, "a"); err != nil {
		t.Fatal(err)
	}
	st.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := st.Save(a, ChoixAbsent, 0, "a"); err != ErrClosed {
		t.Fatalf("après la date limite: %v", err)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	dl := time.Now().Add(time.Hour)
	st, _ := OpenStore(path, 3, dl)
	issued, _, _ := st.ImportMembers([]ImportRow{{Email: "x@ex.fr", Nom: "X", Prenom: "Y"}}, false, "t")
	m, _ := st.MemberByToken(issued[0].Token)
	if err := st.Save(m.ID, ChoixPresent, 0, "x"); err != nil {
		t.Fatal(err)
	}
	st2, err := OpenStore(path, 3, dl)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st2.MemberByToken(issued[0].Token); !ok {
		t.Fatal("token perdu après rechargement")
	}
	if r, ok := st2.ResponseOf(m.ID); !ok || r.Choix != ChoixPresent {
		t.Fatal("réponse perdue après rechargement")
	}
}

func TestImportAndRegen(t *testing.T) {
	st, toks := newTestStore(t, 3, time.Now().Add(time.Hour))
	issued, skipped, err := st.ImportMembers([]ImportRow{{Email: "A@ex.fr"}, {Email: "new@ex.fr", Nom: "N", Prenom: "M"}}, false, "t")
	if err != nil || len(issued) != 1 || skipped != 1 {
		t.Fatalf("issued=%d skipped=%d err=%v", len(issued), skipped, err)
	}
	if _, _, err := st.ImportMembers([]ImportRow{{Email: "pas-un-email"}}, false, "t"); err == nil {
		t.Fatal("email invalide accepté")
	}
	n, err := st.Regenerate("a@ex.fr", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.MemberByToken(toks["a"].Token); ok {
		t.Fatal("ancien token toujours valide")
	}
	if _, ok := st.MemberByToken(n.Token); !ok {
		t.Fatal("nouveau token refusé")
	}
}

func TestParseImport(t *testing.T) {
	rows, err := parseImport([]byte("\xEF\xBB\xBFLastname;Firstname;Email\nDupont;Jean;jean@ex.fr\n"))
	if err != nil || len(rows) != 1 || rows[0].Email != "jean@ex.fr" || rows[0].Nom != "Dupont" || rows[0].Prenom != "Jean" {
		t.Fatalf("%v %+v", err, rows)
	}
	rows, err = parseImport([]byte("jean@ex.fr,Dupont,Jean\nmarie@ex.fr,Martin,Marie\n"))
	if err != nil || len(rows) != 2 || rows[1].Prenom != "Marie" {
		t.Fatalf("%v %+v", err, rows)
	}
	if _, err := parseImport([]byte("nom;prenom\nA;B\n")); err == nil {
		t.Fatal("en-tête sans colonne email accepté")
	}
}

func testServer(t *testing.T) (*Server, *Store, map[string]IssuedToken, http.Handler) {
	t.Helper()
	st, toks := newTestStore(t, 2, time.Now().Add(time.Hour))
	loc, _ := time.LoadLocation("Europe/Paris")
	srv, err := NewServer(Config{BaseURL: "https://ag.hacf.fr", MaxMandats: 2, Deadline: st.Deadline, AGName: "AG", ContactEmail: "c@x.fr",
		AdminGroup: "hacf-ag-admin", HdrUser: "X-Authentik-Username", HdrGroups: "X-Authentik-Groups", Loc: loc}, st)
	if err != nil {
		t.Fatal(err)
	}
	return srv, st, toks, srv.Routes()
}

func TestHTTPFlow(t *testing.T) {
	_, st, toks, h := testServer(t)

	// token invalide -> 404 générique
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/r?t=nimportequoi-nimportequoi", nil))
	if rec.Code != 404 {
		t.Fatalf("code %d", rec.Code)
	}

	// formulaire valide, nom affiché
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/r?t="+toks["a"].Token, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Pa A") {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Referrer-Policy") != "same-origin" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("en-têtes de sécurité manquants")
	}

	// soumission valide
	post := func(origin string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/r", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := post("https://evil.example", url.Values{"t": {toks["a"].Token}, "choix": {"present"}}); rr.Code != 403 {
		t.Fatalf("origine étrangère: %d", rr.Code)
	}
	if rr := post("https://ag.hacf.fr", url.Values{"t": {toks["a"].Token}, "choix": {"present"}}); rr.Code != 303 {
		t.Fatalf("soumission: %d %s", rr.Code, rr.Body.String())
	}
	a := id(t, st, toks["a"])
	if r, ok := st.ResponseOf(a); !ok || r.Choix != ChoixPresent {
		t.Fatal("réponse non enregistrée")
	}
	// règle métier -> 422 avec message
	e := id(t, st, toks["e"])
	rr := post("https://ag.hacf.fr", url.Values{"t": {toks["b"].Token}, "choix": {"mandat"}, "mandataire": {itoa(a)}})
	if rr.Code != 303 {
		t.Fatalf("mandat valide refusé: %d", rr.Code)
	}
	rr = post("https://ag.hacf.fr", url.Values{"t": {toks["c"].Token}, "choix": {"mandat"}, "mandataire": {itoa(a)}})
	if rr.Code != 303 {
		t.Fatalf("2e mandat: %d", rr.Code)
	}
	rr = post("https://ag.hacf.fr", url.Values{"t": {toks["d"].Token}, "choix": {"mandat"}, "mandataire": {itoa(a)}})
	if rr.Code != 422 || !strings.Contains(rr.Body.String(), "maximum") {
		t.Fatalf("plafond: %d %s", rr.Code, rr.Body.String())
	}
	_ = e
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestAdminAccess(t *testing.T) {
	_, _, _, h := testServer(t)
	get := func(path string, hdr map[string]string) int {
		req := httptest.NewRequest("GET", path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := get("/admin", nil); c != 403 {
		t.Fatalf("sans en-têtes: %d", c)
	}
	if c := get("/admin", map[string]string{"X-Authentik-Username": "ludo", "X-Authentik-Groups": "autre|users"}); c != 403 {
		t.Fatalf("mauvais groupe: %d", c)
	}
	if c := get("/admin", map[string]string{"X-Authentik-Username": "ludo", "X-Authentik-Groups": "users|hacf-ag-admin"}); c != 200 {
		t.Fatalf("bon groupe: %d", c)
	}
	if c := get("/admin/export.csv", map[string]string{"X-Authentik-Username": "ludo", "X-Authentik-Groups": "hacf-ag-admin"}); c != 200 {
		t.Fatalf("export: %d", c)
	}

	// import : POST sans origine refusé, avec origine accepté, renvoie email;name;firstname;other
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "m.csv")
	fw.Write([]byte("email;nom;prenom\nzz@ex.fr;Zed;Zoe\n"))
	mw.Close()
	do := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/admin/import", bytes.NewReader(buf.Bytes()))
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("X-Authentik-Username", "ludo")
		req.Header.Set("X-Authentik-Groups", "hacf-ag-admin")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := do(""); rr.Code != 403 {
		t.Fatalf("import sans origine: %d", rr.Code)
	}
	rr := do("https://ag.hacf.fr")
	line := strings.TrimSpace(rr.Body.String())
	parts := strings.Split(line, ";")
	if rr.Code != 200 || len(parts) != 4 || parts[0] != "zz@ex.fr" || parts[1] != "Zed" || parts[2] != "Zoe" || len(parts[3]) < 40 {
		t.Fatalf("format Dolibarr: %d %q", rr.Code, line)
	}
}

func TestCSVInjection(t *testing.T) {
	if csvSafe("=cmd()") != "'=cmd()" || csvSafe("ok") != "ok" {
		t.Fatal("csvSafe")
	}
}

func TestOpenTracking(t *testing.T) {
	_, st, toks, h := testServer(t)
	stats := st.Stats()
	if stats.Ouverts != 0 {
		t.Fatalf("ouverts au départ: %d", stats.Ouverts)
	}
	// lien invalide : rien d'enregistré
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/r?t=nimportequoi-nimportequoi", nil))
	// deux ouvertures de A, une de B
	for _, n := range []string{"a", "a", "b"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/r?t="+toks[n].Token, nil))
	}
	// la page de confirmation (?ok=1) ne compte pas comme une ouverture
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/r?t="+toks["b"].Token+"&ok=1", nil))
	stats = st.Stats()
	if stats.Ouverts != 2 {
		t.Fatalf("ouverts = %d, attendu 2", stats.Ouverts)
	}
	for _, r := range stats.Rows {
		switch r.Member.Email {
		case "a@ex.fr":
			if r.Member.Opens != 2 || r.Member.FirstOpenAt.IsZero() {
				t.Fatalf("A: %+v", r.Member)
			}
		case "b@ex.fr":
			if r.Member.Opens != 1 {
				t.Fatalf("B: %+v", r.Member)
			}
		case "c@ex.fr":
			if r.Member.Opens != 0 || !r.Member.FirstOpenAt.IsZero() {
				t.Fatalf("C: %+v", r.Member)
			}
		}
	}
}

func TestResetDoubleValidation(t *testing.T) {
	_, st, toks, h := testServer(t)
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://ag.hacf.fr")
		req.Header.Set("X-Authentik-Username", "ludo")
		req.Header.Set("X-Authentik-Groups", "hacf-ag-admin")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	get := func(path string, groups string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("X-Authentik-Username", "ludo")
		req.Header.Set("X-Authentik-Groups", groups)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := get("/admin/reset", "autre"); c != 403 {
		t.Fatalf("page reset sans droit: %d", c)
	}
	if c := get("/admin/reset", "hacf-ag-admin"); c != 200 {
		t.Fatalf("page reset: %d", c)
	}
	// un POST sans origine est refusé (CSRF)
	req := httptest.NewRequest("POST", "/admin/reset", strings.NewReader("confirm=REINITIALISER&ack=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Authentik-Username", "ludo")
	req.Header.Set("X-Authentik-Groups", "hacf-ag-admin")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 || st.Stats().Total != 6 {
		t.Fatalf("reset sans origine: %d", rr.Code)
	}
	// mot faux, case non cochée : rien n'est effacé
	for _, body := range []string{"confirm=REINITIALISER", "ack=1", "confirm=oui&ack=1", ""} {
		if rr := post("/admin/reset", body); rr.Code != 303 || st.Stats().Total != 6 {
			t.Fatalf("%q: code %d, adhérents %d", body, rr.Code, st.Stats().Total)
		}
	}
	// réponse enregistrée avant le reset
	m, _ := st.MemberByToken(toks["a"].Token)
	if err := st.Save(m.ID, ChoixPresent, 0, "test"); err != nil {
		t.Fatal(err)
	}
	// double validation correcte : tout est effacé, copie conservée, ancien lien invalide
	if rr := post("/admin/reset", "confirm=REINITIALISER&ack=1"); rr.Code != 303 {
		t.Fatalf("reset: %d", rr.Code)
	}
	if s := st.Stats(); s.Total != 0 || s.Repondus != 0 {
		t.Fatalf("après reset: %+v", s)
	}
	if _, ok := st.MemberByToken(toks["a"].Token); ok {
		t.Fatal("ancien lien encore valide")
	}
	baks, _ := filepath.Glob(filepath.Join(filepath.Dir(st.path), "state.reset-*.bak"))
	if len(baks) != 1 {
		t.Fatalf("sauvegardes: %v", baks)
	}
	// on peut réimporter ensuite
	if _, _, err := st.ImportMembers([]ImportRow{{Email: "n@ex.fr", Nom: "N", Prenom: "Nn"}}, false, "test"); err != nil {
		t.Fatal(err)
	}
	if st.Stats().Total != 1 {
		t.Fatal("réimport après reset")
	}
}

func TestAdminFilter(t *testing.T) {
	_, st, toks, h := testServer(t)
	a, _ := st.MemberByToken(toks["a"].Token)
	b, _ := st.MemberByToken(toks["b"].Token)
	if err := st.Save(a.ID, ChoixPresent, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(b.ID, ChoixAbsent, 0, "test"); err != nil {
		t.Fatal(err)
	}
	st.MarkOpened(a.ID)
	st.MarkOpened(toks2id(t, st, toks["c"]))
	rows := st.Stats().Rows
	count := func(q, f string) int { return len(filterRows(rows, q, f)) }
	if count("", "") != 6 || count("", "present") != 1 || count("", "absent") != 1 || count("", "sans") != 4 {
		t.Fatal("filtres de statut")
	}
	if count("", "relance") != 1 || count("", "jamais") != 3 {
		t.Fatalf("relance=%d jamais=%d", count("", "relance"), count("", "jamais"))
	}
	if count("pa a", "") != 1 || count("B@EX.FR", "") != 1 || count("zzz", "") != 0 || count("p", "sans") != 4 {
		t.Fatal("recherche")
	}
	if fold("Éloïse-Çà") != "eloise ca" {
		t.Fatalf("fold: %q", fold("Éloïse-Çà"))
	}
	req := httptest.NewRequest("GET", "/admin?q=pa+a&f=present", nil)
	req.Header.Set("X-Authentik-Username", "ludo")
	req.Header.Set("X-Authentik-Groups", "hacf-ag-admin")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "a@ex.fr") || strings.Contains(body, "b@ex.fr") || !strings.Contains(body, "1 sur 6") {
		t.Fatalf("page filtrée: %d", rr.Code)
	}
}

func toks2id(t *testing.T, st *Store, tok IssuedToken) int {
	t.Helper()
	m, ok := st.MemberByToken(tok.Token)
	if !ok {
		t.Fatal("token inconnu")
	}
	return m.ID
}
