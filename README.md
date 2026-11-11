# HACF-AG

Application web légère pour organiser l'assemblée générale d'une association : recueil des
réponses à la convocation (présent, absent, absent avec procuration) puis votes électroniques
anonymes le jour de l'assemblée.

Écrite en Go, sans dépendance externe (bibliothèque standard uniquement), elle tourne dans une
image Docker minimale et conserve son état dans un simple fichier JSON.

## Fonctionnement

**Avant l'assemblée**

1. Les adhérents sont importés (depuis Dolibarr via son API REST, ou depuis un fichier CSV).
   Chaque adhérent reçoit un **lien personnel** contenant un jeton aléatoire, impossible à deviner.
   Seul l'empreinte (SHA-256) du jeton est conservée.
2. La convocation est envoyée par e-mail (publipostage) avec ce lien.
3. L'adhérent ouvre son lien et indique s'il sera **présent**, **absent** ou **absent avec
   procuration** (en choisissant un adhérent déjà présent). Il peut modifier sa réponse jusqu'à la date limite.
4. L'administration suit les réponses en direct : tuiles de synthèse, barre de répartition,
   liste des adhérents avec recherche et filtres (présents, absents, procurations, sans réponse,
   lien jamais ouvert…), export CSV.

**Le jour de l'assemblée**

1. Le bureau crée des **scrutins** : décision (Pour / Contre / Abstention) ou choix multiple
   (candidats, options, avec vote blanc).
2. À l'ouverture du premier scrutin, les réponses sont figées et les **voix sont fixées** :
   une voix par adhérent présent, plus les procurations portées.
3. Le même lien personnel donne alors accès à la **salle de vote**. L'adhérent vote, puis la page
   se met à jour toute seule pour le scrutin suivant.
4. À la clôture, les résultats sont affichés à tous (totaux, pourcentages, option en tête ou
   « adopté / rejeté »). Ils sont masqués tant que le vote est ouvert.

## Anonymat des votes

- Un scrutin ne conserve que des **totaux par option**.
- L'application note seulement **quelles voix ont été utilisées** (émargement, pour empêcher de voter
  deux fois), jamais le choix associé.
- Aucune date n'est enregistrée avec un vote.
- Un vote validé ne peut pas être modifié.

Les indications de quorum et de majorité affichées côté administration sont **indicatives** : la
décision finale relève du bureau, selon les statuts de l'association.

## Configuration

Toute la configuration passe par des variables d'environnement (voir `.env.example`).

| Variable | Obligatoire | Description |
|---|---|---|
| `BASE_URL` | oui | URL publique de l'application, utilisée pour fabriquer les liens (ex. `https://ag.example.org`) |
| `DEADLINE` | oui | Date limite de réponse, au format RFC 3339 (ex. `2026-12-18T23:59:00+01:00`) |
| `LISTEN` | non | Adresse d'écoute (défaut `:8080`) |
| `DATA_DIR` | non | Dossier de l'état (défaut `/data`) |
| `MAX_MANDATS` | non | Nombre maximal de procurations par personne (défaut `3`) |
| `AG_NAME` | non | Titre affiché en haut des pages |
| `AG_DATE_TEXT` | non | Date de l'assemblée en toutes lettres, affichée sous le titre |
| `CONTACT_EMAIL` | non | Adresse de contact affichée en bas des pages |
| `ADMIN_GROUP` | non | Groupe autorisé à accéder à `/admin` (si vide, l'administration est désactivée) |
| `HDR_USER` | non | En-tête portant le nom de l'utilisateur authentifié (défaut `X-Authentik-Username`) |
| `HDR_GROUPS` | non | En-tête portant ses groupes (défaut `X-Authentik-Groups`) |
| `DOLIBARR_URL` | non | URL de Dolibarr, pour la synchronisation des adhérents |
| `DOLIBARR_API_KEY` | non | Clé API Dolibarr en **lecture seule** (droit de lecture des adhérents uniquement) |
| `DOLIBARR_HOST` | non | En-tête `Host` à envoyer si Dolibarr est derrière un reverse proxy |

Sans `DOLIBARR_URL` et `DOLIBARR_API_KEY`, le bouton de synchronisation n'apparaît pas : on peut
alors importer un fichier.

## Déploiement

L'image est publiée par GitHub Actions : `ghcr.io/barto95100/hacf-ag`.

```yaml
services:
  ag:
    image: ghcr.io/barto95100/hacf-ag:latest
    restart: unless-stopped
    env_file: .env
    volumes:
      - ./data:/data
```

- Le conteneur s'exécute avec l'utilisateur `10001` : le dossier `data/` doit lui appartenir
  (`chown 10001:10001 data`).
- L'application doit être placée **derrière un reverse proxy** qui gère le HTTPS. Ne publiez pas son
  port directement.
- Le contrôle de santé est disponible sur `GET /healthz`.

### Authentification de l'administration

L'application ne gère aucun mot de passe. Les pages `/admin` exigent que le reverse proxy
authentifie l'utilisateur (forward auth : authentik, Authelia, etc.) et transmette son nom et ses
groupes dans les en-têtes `HDR_USER` et `HDR_GROUPS`. L'accès est accordé si l'utilisateur appartient
à `ADMIN_GROUP`.

Les routes publiques (`/r`, `/v`) ne doivent pas être soumises à cette authentification : elles
sont protégées par le jeton personnel.

> **Important** : seul le reverse proxy doit pouvoir joindre l'application. Si elle était
> accessible directement, quelqu'un pourrait forger les en-têtes d'authentification.

## Routes

| Route | Rôle |
|---|---|
| `GET /r?t=…` | Page de réponse de l'adhérent |
| `GET /v?t=…` | Salle de vote |
| `GET /admin` | Tableau de bord, adhérents, import / synchronisation |
| `GET /admin/scrutins` | Création, ouverture et clôture des scrutins |
| `GET /admin/export.csv` | Export de la feuille de présence |
| `GET /admin/reset` | Remise à zéro (double validation, sauvegarde préalable de l'état) |
| `GET /healthz` | Contrôle de santé |

## Sécurité

- Jetons aléatoires, stockés sous forme d'empreinte SHA-256.
- Protection CSRF (contrôle des en-têtes `Origin` / `Referer`) sur toutes les actions.
- Politique de sécurité de contenu stricte : aucun script côté navigateur. Les filtres, les menus
  déroulants et le rafraîchissement automatique fonctionnent côté serveur.
- Aucune donnée n'est envoyée à un service tiers ; l'état reste dans `DATA_DIR`.
- La remise à zéro écrit d'abord une copie de sauvegarde de l'état.

## Données personnelles

Le fichier `data/state.json` contient les noms, adresses e-mail et réponses des adhérents. Il ne
doit **jamais** être versionné ni publié (il est exclu par `.gitignore`). Sauvegardez-le et
supprimez-le une fois l'assemblée terminée, selon la politique de conservation de l'association.

## Développement

```bash
go vet ./...
go test ./...
go run .          # nécessite BASE_URL et DEADLINE
```

Construire l'image en local :

```bash
docker build -t hacf-ag .
```

L'intégration continue (`.github/workflows/docker.yml`) lance les tests puis construit et publie
l'image à chaque push sur `main`.
