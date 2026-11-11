package main

import (
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestScrutinAnonymeEtPoids(t *testing.T) {
	st, toks := newTestStore(t, 3, time.Now().Add(time.Hour))
	ids := map[string]int{}
	for k, tk := range toks {
		ids[k] = id(t, st, tk)
	}
	// a présent ; b et c donnent procuration à a ; d absent ; e présent ; f sans réponse
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.Save(ids["a"], ChoixPresent, 0, "t"))
	must(st.Save(ids["e"], ChoixPresent, 0, "t"))
	must(st.Save(ids["b"], ChoixMandat, ids["a"], "t"))
	must(st.Save(ids["c"], ChoixMandat, ids["a"], "t"))
	must(st.Save(ids["d"], ChoixAbsent, 0, "t"))

	sc, err := st.CreateScrutin("Comptes", "", KindDecision, nil, "adm")
	must(err)
	sc2, err := st.CreateScrutin("Secrétaire", "", KindChoix, []string{"Alice", "Bob", "alice", ""}, "adm")
	must(err)
	if len(sc2.Options) != 3 || sc2.Options[2] != "Vote blanc" {
		t.Fatalf("options: %v", sc2.Options)
	}
	if _, err := st.CreateScrutin("", "", KindDecision, nil, "adm"); err == nil {
		t.Fatal("titre vide accepté")
	}
	// vote impossible avant ouverture
	if _, err := st.Cast(ids["a"], sc.ID, 0, nil); err == nil {
		t.Fatal("vote avant ouverture")
	}
	must(st.OpenScrutin(sc.ID, "adm"))
	if err := st.OpenScrutin(sc2.ID, "adm"); err == nil {
		t.Fatal("deux scrutins ouverts")
	}
	// les réponses sont figées
	if err := st.Save(ids["f"], ChoixPresent, 0, "t"); err == nil {
		t.Fatal("réponse modifiable pendant le vote")
	}
	// voix : a(3 : a, b, c) + e(1) = 4
	if v := st.Scrutins()[0]; v.Voix != 4 {
		t.Fatalf("voix = %d", v.Voix)
	}
	// d (absent) et f (sans réponse) n'ont pas de voix
	for _, k := range []string{"d", "f"} {
		if rm := st.RoomFor(ids[k]); rm.HasVoices {
			t.Fatalf("%s a des voix", k)
		}
		if _, err := st.Cast(ids[k], sc.ID, 0, nil); err == nil {
			t.Fatalf("%s a voté sans voix", k)
		}
	}
	// b a donné procuration : il ne peut pas voter lui-même
	if _, err := st.Cast(ids["b"], sc.ID, 0, nil); err == nil {
		t.Fatal("mandant a voté")
	}
	// a : Pour pour lui et c, mais Contre pour b
	rm := st.RoomFor(ids["a"])
	if !rm.Mine || len(rm.Mandants) != 2 {
		t.Fatalf("salle de a: %+v", rm)
	}
	n, err := st.Cast(ids["a"], sc.ID, 0, map[int]int{ids["b"]: 1, ids["c"]: 99})
	must(err)
	if n != 3 {
		t.Fatalf("voix de a: %d", n)
	}
	if _, err := st.Cast(ids["a"], sc.ID, 0, nil); err == nil {
		t.Fatal("double vote")
	}
	if rm := st.RoomFor(ids["a"]); !rm.Voted {
		t.Fatal("a devrait être marqué comme ayant voté")
	}
	if _, err := st.Cast(ids["e"], sc.ID, 7, nil); err == nil {
		t.Fatal("choix hors liste")
	}
	if _, err := st.Cast(ids["e"], sc.ID, 2, nil); err != nil {
		t.Fatal(err)
	}
	must(st.CloseScrutin(sc.ID, "adm"))
	v := st.Scrutins()[0]
	// a(Pour) + c(suit a : Pour) = 2 Pour ; b = 1 Contre ; e = 1 Abstention
	if v.Counts[0] != 2 || v.Counts[1] != 1 || v.Counts[2] != 1 || v.Exprimees != 4 || !v.Majorite {
		t.Fatalf("résultats: %+v", v)
	}
	// seuil : 25 % de 6 membres = 2 voix
	if v.Seuil != 2 || !v.Quorum {
		t.Fatalf("seuil %d quorum %v", v.Seuil, v.Quorum)
	}
	// anonymat : l'état écrit ne contient ni choix par personne ni horodatage de vote
	raw, _ := os.ReadFile(st.path)
	js := string(raw)
	if strings.Contains(js, `"ballots"`) || strings.Contains(js, `"choix_de"`) {
		t.Fatal("bulletins nominatifs dans l'état")
	}
	for _, e := range st.st.Audit {
		if e.Action == "a_vote" && (strings.Contains(e.Detail, "Pour") || strings.Contains(e.Detail, "Contre")) {
			t.Fatalf("audit révèle le choix: %q", e.Detail)
		}
	}
	// un scrutin ouvert/clos ne peut pas être supprimé ; un brouillon oui
	if err := st.DeleteScrutin(sc.ID, "adm"); err == nil {
		t.Fatal("suppression d'un scrutin clos")
	}
	must(st.DeleteScrutin(sc2.ID, "adm"))
	// la remise à zéro efface aussi les scrutins
	if _, err := st.Reset("adm"); err != nil {
		t.Fatal(err)
	}
	if len(st.Scrutins()) != 0 {
		t.Fatal("scrutins après reset")
	}
}

func TestScrutinHTTP(t *testing.T) {
	_, st, toks, h := testServer(t)
	a := id(t, st, toks["a"])
	if err := st.Save(a, ChoixPresent, 0, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateScrutin("Budget", "", KindDecision, nil, "adm"); err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		return rr
	}
	if rr := get("/v?t=nimportequoi-nimportequoi"); rr.Code != 404 {
		t.Fatalf("token invalide: %d", rr.Code)
	}
	rr := get("/v?t=" + toks["a"].Token)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Aucun vote") || !strings.Contains(rr.Body.String(), `http-equiv="refresh"`) {
		t.Fatalf("salle vide: %d", rr.Code)
	}
	if err := st.OpenScrutin(1, "adm"); err != nil {
		t.Fatal(err)
	}
	rr = get("/v?t=" + toks["a"].Token)
	if !strings.Contains(rr.Body.String(), "Valider mon vote") || strings.Contains(rr.Body.String(), `http-equiv="refresh"`) {
		t.Fatalf("formulaire de vote: %s", rr.Body.String())
	}
	post := func(tok string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://ag.hacf.fr")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if rr := post(toks["a"].Token, url.Values{"t": {toks["a"].Token}, "s": {"1"}, "c": {"5"}}); rr.Code != 422 {
		t.Fatalf("choix invalide: %d", rr.Code)
	}
	if rr := post(toks["a"].Token, url.Values{"t": {toks["a"].Token}, "s": {"1"}, "c": {"0"}}); rr.Code != 303 {
		t.Fatalf("vote: %d", rr.Code)
	}
	if rr := post(toks["a"].Token, url.Values{"t": {toks["a"].Token}, "s": {"1"}, "c": {"1"}}); rr.Code != 422 {
		t.Fatalf("double vote: %d", rr.Code)
	}
	// admin
	areq := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		var req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://ag.hacf.fr")
		req.Header.Set("X-Authentik-Username", "ludo")
		req.Header.Set("X-Authentik-Groups", "hacf-ag-admin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	rr = areq("GET", "/admin/scrutins", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "1</b> voix exprimées") {
		t.Fatalf("admin scrutins: %d %s", rr.Code, rr.Body.String())
	}
	req := httptest.NewRequest("GET", "/admin/scrutins", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("admin sans droit: %d", w.Code)
	}
	if rr := areq("POST", "/admin/scrutins/close", url.Values{"id": {"1"}}); rr.Code != 303 {
		t.Fatalf("close: %d", rr.Code)
	}
	rr = areq("GET", "/admin/scrutins", nil)
	if !strings.Contains(rr.Body.String(), "Clos") || !strings.Contains(rr.Body.String(), strconv.Itoa(1)) {
		t.Fatal("résultats non affichés")
	}
	if rr := areq("POST", "/admin/scrutins/create", url.Values{"titre": {"Cotisations"}, "kind": {"decision"}}); rr.Code != 303 {
		t.Fatalf("create: %d", rr.Code)
	}
	if len(st.Scrutins()) != 2 {
		t.Fatal("scrutin non créé")
	}
}

func TestFormMasqueApresDebutDuVote(t *testing.T) {
	_, st, toks, h := testServer(t)
	a, b := id(t, st, toks["a"]), id(t, st, toks["b"])
	if err := st.Save(a, ChoixPresent, 0, "t"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(b, ChoixAbsent, 0, "t"); err != nil {
		t.Fatal(err)
	}
	get := func(tok string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/r?t="+tok, nil))
		return rr
	}
	// avant le vote : formulaire normal, aucun accès à la salle de vote
	rr := get(toks["a"].Token)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Enregistrer ma réponse") || strings.Contains(rr.Body.String(), "salle de vote") {
		t.Fatalf("avant le vote: %d", rr.Code)
	}
	st.CreateScrutin("Budget", "", KindDecision, nil, "adm")
	if err := st.OpenScrutin(1, "adm"); err != nil {
		t.Fatal(err)
	}
	// après : le présent est envoyé directement dans la salle de vote
	rr = get(toks["a"].Token)
	if rr.Code != 303 || !strings.HasPrefix(rr.Header().Get("Location"), "/v?t=") {
		t.Fatalf("redirection: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	// l'absent est lui aussi envoyé dans la salle de vote, jamais sur le formulaire
	rr = get(toks["b"].Token)
	if rr.Code != 303 || !strings.HasPrefix(rr.Header().Get("Location"), "/v?t=") {
		t.Fatalf("absent: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	// la salle de vote n'a plus de lien vers le formulaire de réponse
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v?t="+toks["a"].Token, nil))
	if strings.Contains(rr.Body.String(), "Retour à ma réponse") {
		t.Fatal("lien de retour présent pendant le vote")
	}
}

func TestResultatsVisiblesApresCloture(t *testing.T) {
	_, st, toks, h := testServer(t)
	a := id(t, st, toks["a"])
	if err := st.Save(a, ChoixPresent, 0, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateScrutin("Budget 2027", "", KindDecision, nil, "adm"); err != nil {
		t.Fatal(err)
	}
	if err := st.OpenScrutin(1, "adm"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Cast(a, 1, 0, nil); err != nil {
		t.Fatal(err)
	}
	get := func(path string) string {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		return rr.Body.String()
	}
	if b := get("/v?t=" + toks["a"].Token); strings.Contains(b, "Résultats des votes clos") {
		t.Fatal("résultats visibles pendant le vote")
	}
	if err := st.CloseScrutin(1, "adm"); err != nil {
		t.Fatal(err)
	}
	if b := get("/v?t=" + toks["a"].Token); !strings.Contains(b, "Résultats des votes clos") || !strings.Contains(b, "Budget 2027") || !strings.Contains(b, "Adopté") || !strings.Contains(b, `class="win"`) {
		t.Fatalf("résultats absents après clôture: %s", b)
	}
	// un adhérent sans réponse est envoyé vers la salle, où les résultats sont visibles
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/r?t="+toks["b"].Token, nil))
	if rr.Code != 303 {
		t.Fatalf("redirection absent: %d", rr.Code)
	}
	if b := get("/v?t=" + toks["b"].Token); !strings.Contains(b, "Budget 2027") {
		t.Fatal("résultats invisibles pour un adhérent sans voix")
	}
}

func TestIssueScrutin(t *testing.T) {
	mk := func(kind string, opts []string, counts []int, expr int) ScrutinView {
		v := ScrutinView{Scrutin: Scrutin{Kind: kind, Options: opts, Statut: StatutClos, Counts: counts}, Exprimees: expr}
		v.issue()
		return v
	}
	dec := []string{"Pour", "Contre", "Abstention"}
	if v := mk(KindDecision, dec, []int{5, 2, 9}, 16); v.IssueTon != "ok" || !strings.HasPrefix(v.Issue, "Adopté") {
		t.Fatalf("adopté: %+v", v)
	}
	if v := mk(KindDecision, dec, []int{2, 5, 0}, 7); v.IssueTon != "ko" || !strings.HasPrefix(v.Issue, "Rejeté") {
		t.Fatalf("rejeté: %+v", v)
	}
	if v := mk(KindDecision, dec, []int{3, 3, 4}, 10); !strings.HasPrefix(v.Issue, "Égalité") || len(v.Gagnants) != 2 {
		t.Fatalf("égalité: %+v", v)
	}
	// l'abstention (ou le vote blanc) ne peut jamais l'emporter
	if v := mk(KindDecision, dec, []int{0, 0, 6}, 6); len(v.Gagnants) != 0 || v.IssueTon != "neutre" {
		t.Fatalf("abstentions seules: %+v", v)
	}
	ch := []string{"Alice", "Bob", "Vote blanc"}
	if v := mk(KindChoix, ch, []int{4, 7, 20}, 31); !strings.Contains(v.Issue, "Bob") || len(v.Gagnants) != 1 || v.Gagnants[0] != 1 {
		t.Fatalf("choix: %+v", v)
	}
	if v := mk(KindChoix, ch, []int{0, 0, 0}, 0); v.Issue != "Aucun vote exprimé." {
		t.Fatalf("vide: %+v", v)
	}
}
