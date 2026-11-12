package main

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpdatePseudosKeepsTokens(t *testing.T) {
	st, toks := newTestStore(t, 3, time.Now().Add(time.Hour))
	rep, err := st.UpdatePseudos([]ImportRow{
		{Email: "A@ex.fr", Pseudo: "  Alpha  "},
		{Email: "b@ex.fr", Pseudo: ""},
		{Email: "inconnu@ex.fr", Pseudo: "Zed"},
	}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 1 || rep.NoPseudo != 1 || len(rep.Unknown) != 1 {
		t.Fatalf("rapport: %+v", rep)
	}
	m, ok := st.MemberByToken(toks["a"].Token)
	if !ok || m.Pseudo != "Alpha" || m.Public() != "Alpha" || m.Nom != "A" {
		t.Fatalf("token a: %v %+v", ok, m)
	}
	mb, ok := st.MemberByToken(toks["b"].Token)
	if !ok || mb.Public() != "B Pb" {
		t.Fatalf("repli nom/prénom: %+v", mb)
	}
	rep, _ = st.UpdatePseudos([]ImportRow{{Email: "a@ex.fr", Pseudo: "Alpha"}}, "t")
	if rep.Unchanged != 1 || rep.Updated != 0 {
		t.Fatalf("unchanged: %+v", rep)
	}
}

func TestUpdatePseudosDuplicate(t *testing.T) {
	st, toks := newTestStore(t, 3, time.Now().Add(time.Hour))
	_, err := st.UpdatePseudos([]ImportRow{{Email: "a@ex.fr", Pseudo: "Neo"}, {Email: "b@ex.fr", Pseudo: "neo"}}, "t")
	if err == nil {
		t.Fatal("doublon accepté")
	}
	if m, _ := st.MemberByToken(toks["a"].Token); m.Pseudo != "" {
		t.Fatal("modification partielle malgré le refus")
	}
	if _, err := st.UpdatePseudos([]ImportRow{{Email: "a@ex.fr"}, {Email: "b@ex.fr"}}, "t"); err != nil {
		t.Fatalf("pseudos vides bloqués: %v", err)
	}
}

func TestParseImportPseudoColumn(t *testing.T) {
	rows, has, err := parseImportFull([]byte("email;pseudo\na@ex.fr;Alpha\n"))
	if err != nil || !has || len(rows) != 1 || rows[0].Pseudo != "Alpha" {
		t.Fatalf("%v %v %+v", err, has, rows)
	}
	_, has, err = parseImportFull([]byte("email;nom;prenom\na@ex.fr;A;B\n"))
	if err != nil || has {
		t.Fatalf("colonne pseudo détectée à tort: %v %v", err, has)
	}
}

func TestPseudoDisplayHTTP(t *testing.T) {
	_, st, toks, h := testServer(t)
	if _, err := st.UpdatePseudos([]ImportRow{{Email: "b@ex.fr", Pseudo: "Bobby"}}, "t"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(id(t, st, toks["b"]), "present", 0, "t"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/r?t="+toks["a"].Token, nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Bobby") || strings.Contains(body, "B Pb") {
		t.Fatalf("liste de procuration: %s", body)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "p.csv")
	fw.Write([]byte("email;pseudo\nc@ex.fr;Charlie\n"))
	mw.Close()
	req := httptest.NewRequest("POST", "/admin/pseudos", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Authentik-Username", "ludo")
	req.Header.Set("X-Authentik-Groups", "hacf-ag-admin")
	req.Header.Set("Origin", "https://ag.hacf.fr")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 || !strings.Contains(rr.Header().Get("Location"), "1+mis+%C3%A0+jour") {
		t.Fatalf("%d %s", rr.Code, rr.Header().Get("Location"))
	}
	if m, _ := st.MemberByToken(toks["c"].Token); m.Pseudo != "Charlie" {
		t.Fatal("pseudo non enregistré")
	}
	// sans Origin : refusé
	req = httptest.NewRequest("POST", "/admin/pseudos", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Authentik-Username", "ludo")
	req.Header.Set("X-Authentik-Groups", "hacf-ag-admin")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("sans origine: %d", rr.Code)
	}
}

func TestPseudoDoublonsEmail(t *testing.T) {
	st, toks := newTestStore(t, 3, time.Now().Add(time.Hour))
	rep, err := st.UpdatePseudos([]ImportRow{{Email: "a@ex.fr", Pseudo: "Un"}, {Email: "A@ex.fr", Pseudo: "Deux"}}, "t")
	if err != nil || rep.Doublons != 1 || rep.Updated != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	if m, _ := st.MemberByToken(toks["a"].Token); m.Pseudo != "Un" {
		t.Fatalf("la première ligne doit gagner: %q", m.Pseudo)
	}
}
