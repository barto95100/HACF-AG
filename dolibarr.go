package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DolibarrClient lit la liste des adhérents via l'API REST de Dolibarr (lecture seule).
type DolibarrClient struct {
	BaseURL    string // ex. http://dolibarr:80  ou  https://dolibarr.hacf.fr
	Key        string // clé API (en-tête DOLAPIKEY), jamais journalisée
	HostHeader string // facultatif : en-tête Host à envoyer (accès interne derrière Traefik)
	HTTP       *http.Client
}

// DolibarrResult résume ce qui a été lu et ce qui sera convoqué.
type DolibarrResult struct {
	Lus       int // adhérents renvoyés par Dolibarr
	Rows      []ImportRow
	Inactifs  int // statut différent de « validé »
	Expires   int // adhésion terminée
	SansEmail int // email absent ou invalide
	Doublons  int // même email déjà vu
}

// flex accepte nombre, chaîne ou null (Dolibarr renvoie selon les versions l'un ou l'autre).
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*f = ""
		return nil
	}
	s = strings.Trim(s, `"`)
	*f = flex(strings.TrimSpace(s))
	return nil
}

type dolMember struct {
	Statut    flex   `json:"statut"`
	Status    flex   `json:"status"`
	DateFin   flex   `json:"datefin"`
	Email     string `json:"email"`
	Lastname  string `json:"lastname"`
	Firstname string `json:"firstname"`
}

func (m dolMember) validated() bool {
	v := string(m.Statut)
	if v == "" {
		v = string(m.Status)
	}
	return v == "1"
}

// endTime renvoie la date de fin d'adhésion ; ok=false si absente (adhésion sans échéance).
func (m dolMember) endTime() (time.Time, bool) {
	if m.DateFin == "" || m.DateFin == "0" {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(string(m.DateFin), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

const dolPage = 100

// Members lit tous les adhérents (pagination) et retient ceux à convoquer :
// statut « validé », adhésion non terminée à la date now, email valide, sans doublon.
func (c *DolibarrClient) Members(ctx context.Context, now time.Time) (DolibarrResult, error) {
	var res DolibarrResult
	base := strings.TrimRight(c.BaseURL, "/")
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		batch, err := c.page(ctx, base, page)
		if err != nil {
			return DolibarrResult{}, err
		}
		for _, m := range batch {
			res.Lus++
			if !m.validated() {
				res.Inactifs++
				continue
			}
			if end, ok := m.endTime(); ok && end.Before(now) {
				res.Expires++
				continue
			}
			email, err := normEmail(m.Email)
			if err != nil {
				res.SansEmail++
				continue
			}
			if seen[email] {
				res.Doublons++
				continue
			}
			seen[email] = true
			res.Rows = append(res.Rows, ImportRow{Email: email, Nom: m.Lastname, Prenom: m.Firstname})
		}
		if len(batch) < dolPage {
			return res, nil
		}
	}
	return DolibarrResult{}, errors.New("trop de pages renvoyées par Dolibarr")
}

func (c *DolibarrClient) page(ctx context.Context, base string, page int) ([]dolMember, error) {
	q := url.Values{}
	q.Set("sortfield", "t.rowid")
	q.Set("sortorder", "ASC")
	q.Set("limit", strconv.Itoa(dolPage))
	q.Set("page", strconv.Itoa(page))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/index.php/members?"+q.Encode(), nil)
	if err != nil {
		return nil, errors.New("URL Dolibarr invalide")
	}
	req.Header.Set("DOLAPIKEY", c.Key)
	req.Header.Set("Accept", "application/json")
	if c.HostHeader != "" {
		req.Host = c.HostHeader
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 25 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("Dolibarr injoignable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, errors.New("lecture de la réponse Dolibarr impossible")
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && page > 0:
		return nil, nil // page vide : fin de la liste
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, errors.New("Dolibarr a refusé la clé API (droits insuffisants ?)")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("Dolibarr a répondu HTTP %d", resp.StatusCode)
	}
	var out []dolMember
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("réponse Dolibarr illisible")
	}
	return out, nil
}
