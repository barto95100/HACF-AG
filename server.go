package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/csv"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

//go:embed templates/*.html
var tplFS embed.FS

type Config struct {
	Listen       string
	BaseURL      string
	DataDir      string
	MaxMandats   int
	Deadline     time.Time
	AGName       string
	AGDateText   string
	ContactEmail string
	AdminGroup   string
	HdrUser      string
	HdrGroups    string
	DolibarrURL  string
	DolibarrKey  string
	DolibarrHost string
	Loc          *time.Location
}

type Server struct {
	cfg   Config
	store *Store
	tpl   *template.Template
}

func NewServer(cfg Config, st *Store) (*Server, error) {
	funcs := template.FuncMap{
		"isWin": func(w []int, i int) bool {
			for _, x := range w {
				if x == i {
					return true
				}
			}
			return false
		},
		"year":  func() int { return time.Now().In(cfg.Loc).Year() },
		"fdate": func(t time.Time) string { return t.In(cfg.Loc).Format("02/01/2006 15:04") },
		"choix": func(c string) string {
			switch c {
			case ChoixPresent:
				return "Présent(e)"
			case ChoixAbsent:
				return "Absent(e)"
			case ChoixMandat:
				return "Absent(e) avec procuration"
			}
			return ""
		},
	}
	t, err := template.New("").Funcs(funcs).ParseFS(tplFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, tpl: t}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /r", s.handleForm)
	mux.HandleFunc("POST /r", s.handleSubmit)
	mux.HandleFunc("GET /v", s.handleRoom)
	mux.HandleFunc("POST /v", s.handleCast)
	mux.HandleFunc("GET /admin/scrutins", s.admin(s.handleScrutins))
	mux.HandleFunc("POST /admin/scrutins/create", s.admin(s.handleScrutinCreate))
	mux.HandleFunc("POST /admin/scrutins/open", s.admin(s.scrutinAction(func(id int, u string) error { return s.store.OpenScrutin(id, u) }, "Le vote est ouvert : les adhérents le voient dans leur salle de vote.")))
	mux.HandleFunc("POST /admin/scrutins/close", s.admin(s.scrutinAction(func(id int, u string) error { return s.store.CloseScrutin(id, u) }, "Le vote est clos : les résultats sont affichés ci-dessous.")))
	mux.HandleFunc("POST /admin/scrutins/delete", s.admin(s.scrutinAction(func(id int, u string) error { return s.store.DeleteScrutin(id, u) }, "Scrutin supprimé.")))
	mux.HandleFunc("GET /admin", s.admin(s.handleAdmin))
	mux.HandleFunc("GET /admin/export.csv", s.admin(s.handleExport))
	mux.HandleFunc("POST /admin/import", s.admin(s.handleImport))
	mux.HandleFunc("POST /admin/regen", s.admin(s.handleRegen))
	mux.HandleFunc("POST /admin/pseudos", s.admin(s.handlePseudos))
	mux.HandleFunc("GET /admin/reset", s.admin(s.handleResetPage))
	mux.HandleFunc("POST /admin/reset", s.admin(s.handleReset))
	mux.HandleFunc("POST /admin/sync/preview", s.admin(s.handleSyncPreview))
	mux.HandleFunc("POST /admin/sync/import", s.admin(s.handleSyncImport))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.notFound(w) })
	return s.secure(mux)
}

// secure ajoute les en-têtes de sécurité et la protection CSRF (contrôle de l'origine) sur les POST.
func (s *Server) secure(next http.Handler) http.Handler {
	base, _ := url.Parse(s.cfg.BaseURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src https://www.hacf.fr; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		if r.Method == http.MethodPost {
			origin := r.Header.Get("Origin")
			if origin == "" {
				if ref := r.Header.Get("Referer"); ref != "" {
					if u, err := url.Parse(ref); err == nil {
						origin = u.Scheme + "://" + u.Host
					}
				}
			}
			want := base.Scheme + "://" + base.Host
			if origin != "" && origin != want {
				http.Error(w, "origine refusée", http.StatusForbidden)
				return
			}
			if origin == "" && strings.HasPrefix(r.URL.Path, "/admin") {
				http.Error(w, "origine absente", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

type page struct {
	AGName, AGDateText, Contact string
	Deadline                    string
	Refresh                     int // rafraîchissement automatique en secondes (0 = aucun)
}

func (s *Server) base() page {
	return page{AGName: s.cfg.AGName, AGDateText: s.cfg.AGDateText, Contact: s.cfg.ContactEmail,
		Deadline: s.cfg.Deadline.In(s.cfg.Loc).Format("02/01/2006 à 15h04")}
}

func (s *Server) notFound(w http.ResponseWriter) {
	s.render(w, http.StatusNotFound, "invalid", s.base())
}

// ---------- Côté adhérent ----------

type formData struct {
	page
	Member      Member
	Token       string
	Current     *Response
	MandataireN string
	Options     []Member
	Closed      bool
	Saved       bool
	Error       string
	Holds       int
	Votable     bool // a répondu « présent » et le vote a commencé
	Started     bool // l'assemblée a commencé : réponses figées
}

func (s *Server) formFor(m Member, token string) formData {
	fd := formData{page: s.base(), Member: m, Token: token, Closed: s.store.Closed()}
	if r, ok := s.store.ResponseOf(m.ID); ok {
		fd.Current = &r
		if r.Choix == ChoixMandat {
			if mm, ok := s.store.MemberByID(r.MandataireID); ok {
				fd.MandataireN = mm.Public()
			}
		}
	}
	fd.Started = s.store.VotingStarted()
	fd.Votable = fd.Started && fd.Current != nil && fd.Current.Choix == ChoixPresent
	fd.Options = s.store.EligibleMandataires(m.ID)
	st := s.store.Stats()
	for _, row := range st.Rows {
		if row.Member.ID == m.ID {
			fd.Holds = row.Holds
		}
	}
	return fd
}

func (s *Server) handleForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")
	m, ok := s.store.MemberByToken(token)
	if !ok {
		s.notFound(w)
		return
	}
	if r.URL.Query().Get("ok") != "1" {
		s.store.MarkOpened(m.ID)
	}
	fd := s.formFor(m, token)
	// une fois l'assemblée commencée, tout le monde n'a plus que la salle de vote
	// (vote en cours pour les présents, résultats pour les autres)
	if fd.Started {
		http.Redirect(w, r, "/v?t="+url.QueryEscape(token), http.StatusSeeOther)
		return
	}
	fd.Saved = r.URL.Query().Get("ok") == "1"
	s.render(w, http.StatusOK, "form", fd)
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	token := r.PostForm.Get("t")
	m, ok := s.store.MemberByToken(token)
	if !ok {
		s.notFound(w)
		return
	}
	choix := r.PostForm.Get("choix")
	mandID, _ := strconv.Atoi(r.PostForm.Get("mandataire"))
	err := s.store.Save(m.ID, choix, mandID, m.Email)
	if err != nil {
		fd := s.formFor(m, token)
		var re RuleError
		if errors.As(err, &re) {
			fd.Error = re.Error()
			s.render(w, http.StatusUnprocessableEntity, "form", fd)
			return
		}
		log.Printf("save: %v", err)
		fd.Error = "Erreur technique, réessayez dans un instant."
		s.render(w, http.StatusInternalServerError, "form", fd)
		return
	}
	http.Redirect(w, r, "/r?t="+url.QueryEscape(token)+"&ok=1", http.StatusSeeOther)
}

// ---------- Côté admin ----------

func (s *Server) admin(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := strings.TrimSpace(r.Header.Get(s.cfg.HdrUser))
		if s.cfg.AdminGroup == "" || user == "" || !hasGroup(r.Header.Get(s.cfg.HdrGroups), s.cfg.AdminGroup) {
			var names []string
			for k := range r.Header {
				if k != "Cookie" && k != "Authorization" {
					names = append(names, k)
				}
			}
			sort.Strings(names)
			log.Printf("admin refusé: chemin=%s groupe_attendu=%q en-tête_utilisateur(%s)=%q en-tête_groupes(%s)=%q en-têtes_reçus=%v",
				r.URL.Path, s.cfg.AdminGroup, s.cfg.HdrUser, user, s.cfg.HdrGroups, r.Header.Get(s.cfg.HdrGroups), names)
			http.Error(w, "accès refusé", http.StatusForbidden)
			return
		}
		next(w, r, user)
	}
}

func hasGroup(header, want string) bool {
	for _, g := range strings.FieldsFunc(header, func(r rune) bool { return r == '|' || r == ',' }) {
		if strings.TrimSpace(g) == want {
			return true
		}
	}
	return false
}

type adminData struct {
	page
	Stats    Stats
	Max      int
	Message  string
	Issued   *IssuedToken
	Link     string
	Actor    string
	Dolibarr bool
	Rows     []Row  // lignes après filtre
	Q        string // recherche
	Filter   string // filtre de statut
	Filtered bool
}

var accentFold = strings.NewReplacer("à", "a", "â", "a", "ä", "a", "é", "e", "è", "e", "ê", "e", "ë", "e",
	"î", "i", "ï", "i", "ô", "o", "ö", "o", "ù", "u", "û", "u", "ü", "u", "ç", "c", "œ", "oe", "-", " ")

func fold(v string) string { return accentFold.Replace(strings.ToLower(v)) }

// filterRows applique la recherche (nom, prénom, email, sans accents) et le filtre de statut.
func filterRows(rows []Row, q, f string) []Row {
	q = fold(q)
	var out []Row
	for _, r := range rows {
		if q != "" && !strings.Contains(fold(r.Member.Nom+" "+r.Member.Prenom+" "+r.Member.Email+" "+r.Member.Pseudo), q) &&
			!strings.Contains(fold(r.Member.Prenom+" "+r.Member.Nom), q) {
			continue
		}
		opened := !r.Member.FirstOpenAt.IsZero()
		keep := true
		switch f {
		case "present":
			keep = r.Response != nil && r.Response.Choix == ChoixPresent
		case "absent":
			keep = r.Response != nil && r.Response.Choix == ChoixAbsent
		case "mandat":
			keep = r.Response != nil && r.Response.Choix == ChoixMandat
		case "sans":
			keep = r.Response == nil
		case "relance":
			keep = r.Response == nil && opened
		case "jamais":
			keep = !opened && r.Response == nil
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request, user string) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if rs := []rune(q); len(rs) > 80 {
		q = string(rs[:80])
	}
	f := r.URL.Query().Get("f")
	switch f {
	case "present", "absent", "mandat", "sans", "relance", "jamais":
	default:
		f = ""
	}
	st := s.store.Stats()
	s.render(w, http.StatusOK, "admin", adminData{page: s.base(), Stats: st, Max: s.cfg.MaxMandats,
		Message: r.URL.Query().Get("msg"), Actor: user, Dolibarr: s.dolibarr() != nil,
		Rows: filterRows(st.Rows, q, f), Q: q, Filter: f, Filtered: q != "" || f != ""})
}

func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request, user string) {
	st := s.store.Stats()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ag-reponses-`+time.Now().In(s.cfg.Loc).Format("2006-01-02")+`.csv"`)
	w.Write([]byte("\xEF\xBB\xBF"))
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	cw.Write([]string{"nom", "prenom", "pseudo", "email", "reponse", "mandataire", "procurations_detenues", "date_reponse", "lien_ouvert_le"})
	for _, row := range st.Rows {
		rep, date, opened := "sans réponse", "", ""
		if !row.Member.FirstOpenAt.IsZero() {
			opened = row.Member.FirstOpenAt.In(s.cfg.Loc).Format("2006-01-02 15:04")
		}
		if row.Response != nil {
			rep = map[string]string{ChoixPresent: "présent", ChoixAbsent: "absent", ChoixMandat: "absent avec procuration"}[row.Response.Choix]
			date = row.Response.UpdatedAt.In(s.cfg.Loc).Format("2006-01-02 15:04")
		}
		cw.Write([]string{csvSafe(row.Member.Nom), csvSafe(row.Member.Prenom), csvSafe(row.Member.Pseudo), csvSafe(row.Member.Email), rep,
			csvSafe(row.Mandataire), strconv.Itoa(row.Holds), date, opened})
	}
	cw.Flush()
	_ = s.store.auditExport(user)
}

func (s *Store) auditExport(actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit(actor, "export", "export CSV des réponses")
	return s.persist()
}

func cleanField(v string) string {
	v = strings.Map(func(r rune) rune {
		if r == ';' || r == '\n' || r == '\r' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, v)
	return strings.TrimSpace(v)
}

func tokensCSV(list []IssuedToken) []byte {
	var b bytes.Buffer
	for _, t := range list {
		fmt.Fprintf(&b, "%s;%s;%s;%s\n", cleanField(t.Email), cleanField(t.Nom), cleanField(t.Prenom), t.Token)
	}
	return b.Bytes()
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request, user string) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		http.Error(w, "fichier trop gros ou invalide", http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "fichier manquant", http.StatusBadRequest)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "lecture impossible", http.StatusBadRequest)
		return
	}
	rows, err := parseImport(data)
	if err != nil {
		http.Redirect(w, r, "/admin?msg="+url.QueryEscape("Import refusé : "+err.Error()), http.StatusSeeOther)
		return
	}
	issued, skipped, err := s.store.ImportMembers(rows, r.FormValue("regen") == "1", user)
	if err != nil {
		http.Redirect(w, r, "/admin?msg="+url.QueryEscape("Import refusé : "+err.Error()), http.StatusSeeOther)
		return
	}
	if len(issued) == 0 {
		http.Redirect(w, r, "/admin?msg="+url.QueryEscape(fmt.Sprintf("Aucun nouveau lien (%d ligne(s) ignorée(s)).", skipped)), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="dolibarr-liens-`+time.Now().In(s.cfg.Loc).Format("2006-01-02")+`.csv"`)
	w.Write(tokensCSV(issued))
}

// handlePseudos met à jour les pseudos depuis un CSV (colonnes email et pseudo), sans toucher aux liens.
func (s *Server) handlePseudos(w http.ResponseWriter, r *http.Request, user string) {
	back := func(msg string) {
		http.Redirect(w, r, "/admin?msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		http.Error(w, "fichier trop gros ou invalide", http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "fichier manquant", http.StatusBadRequest)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "lecture impossible", http.StatusBadRequest)
		return
	}
	rows, hasCol, err := parseImportFull(data)
	if err != nil {
		back("Pseudos refusés : " + err.Error())
		return
	}
	if !hasCol {
		back("Pseudos refusés : aucune colonne « pseudo » dans l'en-tête du fichier (colonnes attendues : email, pseudo).")
		return
	}
	rep, err := s.store.UpdatePseudos(rows, user)
	if err != nil {
		back("Pseudos refusés : " + err.Error())
		return
	}
	msg := fmt.Sprintf("Pseudos : %d mis à jour, %d déjà à jour, %d ligne(s) sans pseudo.", rep.Updated, rep.Unchanged, rep.NoPseudo)
	if rep.Doublons > 0 {
		msg += fmt.Sprintf(" %d ligne(s) en double (même email) : la première a été gardée.", rep.Doublons)
	}
	if n := len(rep.Unknown); n > 0 {
		msg += fmt.Sprintf(" %d email(s) inconnu(s) de l'appli, ignoré(s).", n)
	}
	msg += " Les liens des adhérents n'ont pas changé."
	back(msg)
}

func (s *Server) handleRegen(w http.ResponseWriter, r *http.Request, user string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	t, err := s.store.Regenerate(r.PostForm.Get("email"), user)
	if err != nil {
		http.Redirect(w, r, "/admin?msg="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	link := strings.TrimRight(s.cfg.BaseURL, "/") + "/r?t=" + t.Token
	s.render(w, http.StatusOK, "regen", adminData{page: s.base(), Issued: &t, Link: link, Actor: user})
}

// parseImport lit un CSV (séparateur ; ou ,) avec ou sans en-tête : email, nom, prénom.
func parseImport(data []byte) ([]ImportRow, error) {
	rows, _, err := parseImportFull(data)
	return rows, err
}

// parseImportFull fait de même et indique si une colonne « pseudo » figure dans l'en-tête.
func parseImportFull(data []byte) ([]ImportRow, bool, error) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false, errors.New("fichier vide")
	}
	first := data
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		first = data[:i]
	}
	comma := ','
	if bytes.Count(first, []byte(";")) >= bytes.Count(first, []byte(",")) {
		comma = ';'
	}
	cr := csv.NewReader(bytes.NewReader(data))
	cr.Comma = comma
	cr.LazyQuotes = true
	cr.FieldsPerRecord = -1
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, false, err
	}
	ie, in, ip, ipseudo := 0, 1, 2, -1
	start := 0
	if len(recs) > 0 {
		hasAt := false
		for _, c := range recs[0] {
			if strings.Contains(c, "@") {
				hasAt = true
			}
		}
		if !hasAt {
			start = 1
			ie, in, ip, ipseudo = -1, -1, -1, -1
			for i, c := range recs[0] {
				switch strings.ToLower(strings.TrimSpace(c)) {
				case "email", "e-mail", "mail", "courriel", "e-mail address", "email address":
					ie = i
				case "nom", "lastname", "last name", "name", "surname":
					in = i
				case "prenom", "prénom", "firstname", "first name":
					ip = i
				case "pseudo", "pseudonyme", "login", "username", "identifiant", "surnom", "nickname":
					ipseudo = i
				}
			}
			if ie < 0 {
				return nil, false, errors.New("colonne email introuvable dans l'en-tête")
			}
		}
	}
	get := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	var rows []ImportRow
	for n, rec := range recs[start:] {
		if len(rec) == 0 || (len(rec) == 1 && strings.TrimSpace(rec[0]) == "") {
			continue
		}
		row := ImportRow{Email: get(rec, ie), Nom: get(rec, in), Prenom: get(rec, ip), Pseudo: get(rec, ipseudo)}
		if row.Email == "" {
			return nil, false, fmt.Errorf("ligne %d : email manquant", n+start+1)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, false, errors.New("aucune ligne exploitable")
	}
	return rows, ipseudo >= 0, nil
}

// dolibarr renvoie le client Dolibarr, ou nil si la synchronisation n'est pas configurée.
func (s *Server) dolibarr() *DolibarrClient {
	if s.cfg.DolibarrURL == "" || s.cfg.DolibarrKey == "" {
		return nil
	}
	return &DolibarrClient{BaseURL: s.cfg.DolibarrURL, Key: s.cfg.DolibarrKey, HostHeader: s.cfg.DolibarrHost}
}

func (s *Server) syncFetch(r *http.Request) (DolibarrResult, error) {
	c := s.dolibarr()
	if c == nil {
		return DolibarrResult{}, errors.New("synchronisation Dolibarr non configurée")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	return c.Members(ctx, time.Now())
}

func (s *Server) syncRedirect(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/admin?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleSyncPreview lit Dolibarr et annonce ce qui serait importé, sans rien modifier.
func (s *Server) handleSyncPreview(w http.ResponseWriter, r *http.Request, user string) {
	res, err := s.syncFetch(r)
	if err != nil {
		s.syncRedirect(w, r, "Synchronisation impossible : "+err.Error())
		return
	}
	nouveaux, connus := s.store.CountNew(res.Rows)
	s.syncRedirect(w, r, fmt.Sprintf(
		"Dolibarr : %d adhérent(s) lu(s), %d à convoquer (validés, adhésion en cours) — %d nouveau(x), %d déjà importé(s). Écartés : %d non validé(s), %d adhésion(s) terminée(s), %d sans email valide, %d doublon(s).",
		res.Lus, len(res.Rows), nouveaux, connus, res.Inactifs, res.Expires, res.SansEmail, res.Doublons))
}

// handleSyncImport importe les nouveaux adhérents de Dolibarr et renvoie leurs liens (une seule fois).
func (s *Server) handleSyncImport(w http.ResponseWriter, r *http.Request, user string) {
	res, err := s.syncFetch(r)
	if err != nil {
		s.syncRedirect(w, r, "Synchronisation impossible : "+err.Error())
		return
	}
	issued, skipped, err := s.store.ImportMembers(res.Rows, false, user)
	if err != nil {
		s.syncRedirect(w, r, "Import refusé : "+err.Error())
		return
	}
	if len(issued) == 0 {
		s.syncRedirect(w, r, fmt.Sprintf("Aucun nouvel adhérent à importer (%d déjà importé(s)).", skipped))
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="dolibarr-liens-`+time.Now().In(s.cfg.Loc).Format("2006-01-02")+`.csv"`)
	w.Write(tokensCSV(issued))
}

const resetWord = "REINITIALISER"

// handleResetPage : première validation, page de confirmation (rien n'est modifié).
func (s *Server) handleResetPage(w http.ResponseWriter, r *http.Request, user string) {
	s.render(w, http.StatusOK, "reset", adminData{page: s.base(), Stats: s.store.Stats(), Actor: user,
		Message: r.URL.Query().Get("msg"), Link: resetWord})
}

// handleReset : seconde validation, mot à saisir et case à cocher, puis effacement.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request, user string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(r.PostForm.Get("confirm")) != resetWord || r.PostForm.Get("ack") != "1" {
		http.Redirect(w, r, "/admin/reset?msg="+url.QueryEscape("Confirmation incomplète : saisissez le mot demandé et cochez la case. Rien n'a été effacé."), http.StatusSeeOther)
		return
	}
	backup, err := s.store.Reset(user)
	if err != nil {
		log.Printf("reset: %v", err)
		http.Redirect(w, r, "/admin?msg="+url.QueryEscape("Remise à zéro impossible : "+err.Error()), http.StatusSeeOther)
		return
	}
	msg := "Remise à zéro effectuée : plus aucun adhérent ni réponse."
	if backup != "" {
		msg += " Une copie de l'ancien état a été conservée sur le serveur (" + filepath.Base(backup) + ")."
	}
	log.Printf("reset effectué par %s", user)
	http.Redirect(w, r, "/admin?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}
