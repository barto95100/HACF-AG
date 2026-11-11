package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ChoixPresent = "present"
	ChoixAbsent  = "absent"
	ChoixMandat  = "mandat"
)

// RuleError est une erreur métier affichable telle quelle à l'utilisateur.
type RuleError string

func (e RuleError) Error() string { return string(e) }

var ErrClosed = RuleError("Les réponses sont closes : la date limite est dépassée.")

type Member struct {
	ID        int       `json:"id"`
	Email     string    `json:"email"`
	Nom       string    `json:"nom"`
	Prenom    string    `json:"prenom"`
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	// Suivi d'ouverture du lien personnel (informatif : un antivirus de messagerie
	// peut ouvrir le lien à la place de l'adhérent).
	FirstOpenAt time.Time `json:"first_open_at,omitempty"`
	LastOpenAt  time.Time `json:"last_open_at,omitempty"`
	Opens       int       `json:"opens,omitempty"`
}

func (m Member) FullName() string { return strings.TrimSpace(m.Prenom + " " + m.Nom) }

type Response struct {
	MemberID     int       `json:"member_id"`
	Choix        string    `json:"choix"`
	MandataireID int       `json:"mandataire_id,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type AuditEntry struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
}

type state struct {
	NextID      int              `json:"next_id"`
	Members     []Member         `json:"members"`
	Responses   map[int]Response `json:"responses"`
	Audit       []AuditEntry     `json:"audit"`
	Scrutins    []Scrutin        `json:"scrutins,omitempty"`
	NextScrutin int              `json:"next_scrutin,omitempty"`
}

type Store struct {
	mu         sync.Mutex
	path       string
	st         state
	byHash     map[string]int // token_hash -> index dans st.Members
	byEmail    map[string]int
	MaxMandats int
	Deadline   time.Time
	Now        func() time.Time
}

func OpenStore(path string, maxMandats int, deadline time.Time) (*Store, error) {
	s := &Store{
		path:       path,
		MaxMandats: maxMandats,
		Deadline:   deadline,
		Now:        time.Now,
	}
	s.st = state{NextID: 1, Responses: map[int]Response{}}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, fmt.Errorf("lecture de %s: %w", path, err)
		}
		if s.st.Responses == nil {
			s.st.Responses = map[int]Response{}
		}
		if s.st.NextID == 0 {
			s.st.NextID = 1
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	s.reindex()
	return s, nil
}

func (s *Store) reindex() {
	s.byHash = map[string]int{}
	s.byEmail = map[string]int{}
	for i, m := range s.st.Members {
		if m.TokenHash != "" {
			s.byHash[m.TokenHash] = i
		}
		s.byEmail[m.Email] = i
	}
}

// persist écrit l'état de façon atomique (fichier temporaire + rename).
func (s *Store) persist() error {
	b, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func (s *Store) audit(actor, action, detail string) {
	s.st.Audit = append(s.st.Audit, AuditEntry{At: s.Now().UTC(), Actor: actor, Action: action, Detail: detail})
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Store) Closed() bool { return s.Now().After(s.Deadline) }

// MemberByToken retrouve un adhérent à partir du token présent dans le lien.
func (s *Store) MemberByToken(token string) (Member, bool) {
	if len(token) < 20 || len(token) > 128 {
		return Member{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.byHash[hashToken(token)]
	if !ok {
		return Member{}, false
	}
	return s.st.Members[i], true
}

// MarkOpened enregistre une ouverture du lien personnel (échec d'écriture ignoré :
// le suivi ne doit jamais empêcher l'adhérent de répondre).
func (s *Store) MarkOpened(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.memberIdx(id)
	if i < 0 {
		return
	}
	now := s.Now()
	m := &s.st.Members[i]
	if m.FirstOpenAt.IsZero() {
		m.FirstOpenAt = now
	}
	m.LastOpenAt = now
	m.Opens++
	_ = s.persist()
}

func (s *Store) memberIdx(id int) int {
	for i, m := range s.st.Members {
		if m.ID == id {
			return i
		}
	}
	return -1
}

func (s *Store) mandatesHeldLocked(id int, except int) int {
	n := 0
	for mid, r := range s.st.Responses {
		if mid == except {
			continue
		}
		if r.Choix == ChoixMandat && r.MandataireID == id {
			n++
		}
	}
	return n
}

// Save enregistre (ou remplace) la réponse d'un adhérent après contrôle des règles.
func (s *Store) Save(memberID int, choix string, mandID int, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Closed() {
		return ErrClosed
	}
	if s.votingStartedLocked() {
		return RuleError("Les réponses sont figées : le vote de l'assemblée a commencé.")
	}
	mi := s.memberIdx(memberID)
	if mi < 0 {
		return RuleError("Adhérent inconnu.")
	}
	held := s.mandatesHeldLocked(memberID, memberID)

	resp := Response{MemberID: memberID, Choix: choix, UpdatedAt: s.Now().UTC()}
	switch choix {
	case ChoixPresent:
	case ChoixAbsent, ChoixMandat:
		if held > 0 {
			return RuleError(fmt.Sprintf("Vous détenez %d procuration(s) : vous ne pouvez pas vous déclarer absent(e) ni donner votre propre pouvoir.", held))
		}
		if choix == ChoixMandat {
			if mandID == memberID {
				return RuleError("Vous ne pouvez pas vous donner procuration à vous-même.")
			}
			ti := s.memberIdx(mandID)
			if ti < 0 {
				return RuleError("Le mandataire choisi n'est pas un adhérent convoqué.")
			}
			if r, ok := s.st.Responses[mandID]; !ok || r.Choix != ChoixPresent {
				return RuleError("Le mandataire choisi n'a pas (encore) indiqué être présent(e) : choisissez un adhérent qui a confirmé sa présence.")
			}
			if s.mandatesHeldLocked(mandID, memberID) >= s.MaxMandats {
				return RuleError(fmt.Sprintf("Le mandataire choisi détient déjà le maximum de %d procurations.", s.MaxMandats))
			}
			resp.MandataireID = mandID
		}
	default:
		return RuleError("Choix invalide.")
	}

	old, had := s.st.Responses[memberID]
	s.st.Responses[memberID] = resp
	detail := choix
	if choix == ChoixMandat {
		detail += " -> " + s.st.Members[s.memberIdx(mandID)].Email
	}
	s.audit(actor, "reponse", s.st.Members[mi].Email+" : "+detail)
	if err := s.persist(); err != nil {
		if had {
			s.st.Responses[memberID] = old
		} else {
			delete(s.st.Responses, memberID)
		}
		s.st.Audit = s.st.Audit[:len(s.st.Audit)-1]
		return fmt.Errorf("enregistrement impossible: %w", err)
	}
	return nil
}

func (s *Store) ResponseOf(memberID int) (Response, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.st.Responses[memberID]
	return r, ok
}

func (s *Store) MemberByID(id int) (Member, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.memberIdx(id)
	if i < 0 {
		return Member{}, false
	}
	return s.st.Members[i], true
}

// EligibleMandataires liste les adhérents que `memberID` peut choisir (noms seulement).
func (s *Store) EligibleMandataires(memberID int) []Member {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Member{}
	for _, m := range s.st.Members {
		if m.ID == memberID {
			continue
		}
		if r, ok := s.st.Responses[m.ID]; !ok || r.Choix != ChoixPresent {
			continue
		}
		if s.mandatesHeldLocked(m.ID, memberID) >= s.MaxMandats {
			continue
		}
		out = append(out, Member{ID: m.ID, Nom: m.Nom, Prenom: m.Prenom})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Nom+" "+out[i].Prenom), strings.ToLower(out[j].Nom+" "+out[j].Prenom)
		return a < b
	})
	return out
}

// ---------- Import / tokens ----------

type ImportRow struct{ Email, Nom, Prenom string }

type IssuedToken struct {
	Email, Nom, Prenom, Token string
}

func normEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e {
		return "", fmt.Errorf("email invalide: %q", e)
	}
	return e, nil
}

// ImportMembers crée les adhérents absents et émet leur token.
// Si regen est vrai, les adhérents déjà présents reçoivent aussi un nouveau token (l'ancien est invalidé).
func (s *Store) ImportMembers(rows []ImportRow, regen bool, actor string) (issued []IssuedToken, skipped int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	backup := s.st
	backup.Members = append([]Member(nil), s.st.Members...)
	backup.Audit = append([]AuditEntry(nil), s.st.Audit...)

	seen := map[string]bool{}
	for _, r := range rows {
		email, e := normEmail(r.Email)
		if e != nil {
			s.st = backup
			s.reindex()
			return nil, 0, e
		}
		if seen[email] {
			skipped++
			continue
		}
		seen[email] = true
		i, exists := s.byEmail[email]
		if exists && !regen {
			skipped++
			continue
		}
		tok, e := newToken()
		if e != nil {
			return nil, 0, e
		}
		if exists {
			s.st.Members[i].TokenHash = hashToken(tok)
			if r.Nom != "" {
				s.st.Members[i].Nom = strings.TrimSpace(r.Nom)
			}
			if r.Prenom != "" {
				s.st.Members[i].Prenom = strings.TrimSpace(r.Prenom)
			}
		} else {
			s.st.Members = append(s.st.Members, Member{
				ID: s.st.NextID, Email: email, Nom: strings.TrimSpace(r.Nom), Prenom: strings.TrimSpace(r.Prenom),
				TokenHash: hashToken(tok), CreatedAt: s.Now().UTC(),
			})
			s.st.NextID++
			i = len(s.st.Members) - 1
		}
		s.byEmail[email] = i
		issued = append(issued, IssuedToken{Email: email, Nom: s.st.Members[i].Nom, Prenom: s.st.Members[i].Prenom, Token: tok})
	}
	s.reindex()
	s.audit(actor, "import", fmt.Sprintf("%d token(s) émis, %d ignoré(s), regen=%v", len(issued), skipped, regen))
	if e := s.persist(); e != nil {
		s.st = backup
		s.reindex()
		return nil, 0, e
	}
	return issued, skipped, nil
}

// Regenerate émet un nouveau token pour un adhérent (l'ancien lien ne marche plus).
func (s *Store) Regenerate(email, actor string) (IssuedToken, error) {
	email, err := normEmail(email)
	if err != nil {
		return IssuedToken{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.byEmail[email]
	if !ok {
		return IssuedToken{}, RuleError("Aucun adhérent avec cet email.")
	}
	tok, err := newToken()
	if err != nil {
		return IssuedToken{}, err
	}
	old := s.st.Members[i].TokenHash
	s.st.Members[i].TokenHash = hashToken(tok)
	s.reindex()
	s.audit(actor, "regen", email)
	if err := s.persist(); err != nil {
		s.st.Members[i].TokenHash = old
		s.st.Audit = s.st.Audit[:len(s.st.Audit)-1]
		s.reindex()
		return IssuedToken{}, err
	}
	m := s.st.Members[i]
	return IssuedToken{Email: m.Email, Nom: m.Nom, Prenom: m.Prenom, Token: tok}, nil
}

// ---------- Vue admin ----------

type Row struct {
	Member     Member
	Response   *Response
	Mandataire string
	Holds      int
}

type Stats struct {
	Total, Repondus, Presents, Absents, Mandats int
	Ouverts                                     int // ont ouvert leur lien
	PctP, PctA, PctM                            int // parts (en %) pour la barre de répartition
	Representes                                 int // présents + mandats
	Rows                                        []Row
	Closed                                      bool
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Total: len(s.st.Members), Closed: s.Closed()}
	for _, m := range s.st.Members {
		row := Row{Member: m, Holds: s.mandatesHeldLocked(m.ID, -1)}
		if !m.FirstOpenAt.IsZero() {
			st.Ouverts++
		}
		if r, ok := s.st.Responses[m.ID]; ok {
			rr := r
			row.Response = &rr
			st.Repondus++
			switch r.Choix {
			case ChoixPresent:
				st.Presents++
			case ChoixAbsent:
				st.Absents++
			case ChoixMandat:
				st.Mandats++
				if i := s.memberIdx(r.MandataireID); i >= 0 {
					row.Mandataire = s.st.Members[i].FullName()
				}
			}
		}
		st.Rows = append(st.Rows, row)
	}
	st.Representes = st.Presents + st.Mandats
	if st.Total > 0 {
		st.PctP, st.PctA, st.PctM = st.Presents*100/st.Total, st.Absents*100/st.Total, st.Mandats*100/st.Total
	}
	sort.Slice(st.Rows, func(i, j int) bool {
		a, b := strings.ToLower(st.Rows[i].Member.Nom+" "+st.Rows[i].Member.Prenom), strings.ToLower(st.Rows[j].Member.Nom+" "+st.Rows[j].Member.Prenom)
		return a < b
	})
	return st
}

// CountNew indique combien d'adresses de rows ne sont pas encore importées.
func (s *Store) CountNew(rows []ImportRow) (nouveaux, connus int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		email, err := normEmail(r.Email)
		if err != nil {
			continue
		}
		if _, ok := s.byEmail[email]; ok {
			connus++
		} else {
			nouveaux++
		}
	}
	return
}

// Reset efface tous les adhérents, réponses et liens (remise à zéro complète).
// Une copie de l'état précédent est écrite à côté du fichier d'état avant l'effacement ;
// si cette copie échoue, rien n'est effacé. Le journal d'audit est conservé.
func (s *Store) Reset(actor string) (backup string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, rerr := os.ReadFile(s.path); rerr == nil {
		backup = filepath.Join(filepath.Dir(s.path), "state.reset-"+s.Now().Format("20060102-150405")+".bak")
		if werr := os.WriteFile(backup, b, 0o600); werr != nil {
			return "", fmt.Errorf("sauvegarde avant remise à zéro impossible: %w", werr)
		}
	}
	n, r := len(s.st.Members), len(s.st.Responses)
	s.st.Members = nil
	s.st.Responses = map[int]Response{}
	s.st.NextID = 1
	s.st.Scrutins, s.st.NextScrutin = nil, 0
	s.reindex()
	s.audit(actor, "reset", fmt.Sprintf("remise à zéro : %d adhérent(s) et %d réponse(s) effacés", n, r))
	if err := s.persist(); err != nil {
		return backup, err
	}
	return backup, nil
}
