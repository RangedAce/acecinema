# HA-First Multi-Site Platform (from scratch)

## Objectif

Construire **from scratch** une plateforme “HA-first” pensée pour fonctionner sur **plusieurs sites** (Dijon, Thory, puis autant que nécessaire), où :

- un service peut avoir plusieurs backends (replicas) répartis sur différents sites,
- si une machine ou un site tombe, le trafic bascule automatiquement,
- l’état “plateforme” (inventaire, routage, santé) est conservé (dans les limites de la Phase I),
- le client continue d’accéder aux services via un endpoint unique (ex: `acecinema.fr`).

Le projet reste **100% Docker**.

> Phase I : les backends sont créés **manuellement** via Portainer.  
> Phase II : déploiement automatique par la plateforme (orchestrateur).

---

## Infra actuelle (topologie)

### Entrée publique

`acecinema.fr` → **VPS OVH** → **Caddy** → **WireGuard hub** → **sites**

### Réseau inter-sites (Hub & Spoke)

- Le VPS héberge un **serveur WireGuard** (hub).
- Chaque site (spoke) se connecte au hub : Dijon, Thory, futurs sites…
- Le VPS est le point central de routage entre sites.

Objectif design : supporter **N sites** (ajouter un site = configuration + déclaration, pas refonte).

---

## Concepts

### Site

Un “site” = un groupe de machines Docker derrière un LAN, connecté au hub WireGuard.

Exemples :

- `site-dijon` (2 PVE)
- `site-thory` (swarm existant, ou un pool Docker dédié)

Le design doit être multi-sites dès le départ.

### Service

Un “service” = un endpoint stable exposé au client (ex: `api.acecinema.fr`, `stream.acecinema.fr`), routé vers un pool de backends.

### Backend

Un “backend” = une instance exécutable (conteneur/stack) d’un service, joignable via :

- une IP WireGuard (ou IP routée via WG)
- un port
- un endpoint de healthcheck (HTTP recommandé)

---

## Phase I (actuelle) : HA réseau + backends manuels

### Ce qui est HA dès maintenant

- **Endpoint public unique** : c’est toujours le VPS/Caddy qui reçoit le trafic.
- **Failover automatique** : Caddy retire les backends HS via healthchecks.
- **Multi-sites** : un service peut avoir des backends à Dijon + Thory + autres.

### Ce qui est manuel (pour l’instant)

- Les backends (conteneurs/stacks) sont **créés manuellement** dans Portainer sur chaque site.
- La plateforme se limite à :
  1) enregistrer/déclarer les backends,
  2) vérifier leur santé,
  3) synchroniser la configuration de routage vers Caddy.

---

## Base de données (Phase I)

Une base centrale (SPOF assumé) stocke :

- inventaire des sites,
- définition des services (domaines, règles),
- liste des backends (addr/port/tags),
- état de santé (dernière vue, statut, métriques simples),
- configuration générée (ou versionnée) pour le LB.

**Important :** la redondance de la DB est repoussée à plus tard (Phase II).

Conséquence :

- si la DB tombe, les backends déjà lancés peuvent continuer,
- mais la plateforme ne peut plus *adapter* le routage/placement automatiquement.

---

## Routage & Load Balancing (Caddy sur VPS)

### Rôle du VPS

Le VPS est :

- **edge gateway** (TLS, domaines, headers, rate-limit éventuel),
- **load balancer global** (répartition + failover),
- **point de routage inter-sites** (via WireGuard hub).

### Principe

Pour chaque service public, Caddy a une liste d’upstreams :

- `10.200.x.y:port` (IP WireGuard d’un backend)
- répartis sur plusieurs sites

Caddy assure :

- healthchecks réguliers
- suppression automatique des backends HS
- réintégration quand ils reviennent

Objectif : si un site tombe, le trafic repart vers les sites restants.

---

## Déclaration d’un backend (Phase I)

### Étape 1 — Déployer le backend (manuel)

Créer une stack/containeur dans Portainer sur le site concerné.

Exigences minimales :

- backend accessible depuis le VPS via l’IP WireGuard (ou route WG)
- port clairement exposé/routé
- endpoint de healthcheck (HTTP recommandé)

### Étape 2 — Enregistrer le backend côté plateforme

Ajouter une entrée (API/DB/YAML) du type :

- `service = api`
- `site = dijon`
- `addr = 10.200.0.23`
- `port = 8080`
- `health = http://10.200.0.23:8080/health`

### Étape 3 — Propagation vers Caddy

Un composant “config-sync” :

- génère la config Caddy (ou un fragment)
- recharge Caddy sans downtime
- maintient un pool d’upstreams par service

---

## Définition de “coupure minimale”

- En HTTP : une requête peut échouer pile au moment où un backend meurt, mais les suivantes repartent vers un backend sain.
- En connexions longues (WebSocket/streaming) : reconnexion possible selon l’application ; la plateforme vise à la rendre rapide mais ne peut pas rendre une connexion “immortelle”.

---

## Limites Phase I (acceptées)

- DB centrale non redondée (SPOF)
- backends déclarés manuellement (Portainer)
- pas encore d’orchestrateur/scheduler automatique
- stateful avancé (volumes distribués, DB HA, leader election) repoussé

---

## Phase II (future) : automatisation + vraie tolérance aux pannes

Objectifs :

- découverte/inscription automatique des sites
- agent léger par site (reporting, auto-register)
- déploiement automatique (Docker API / Portainer API)
- placement (scheduler) + auto-healing (reschedule)
- gestion du stateful : stockage distribué + DB HA + election

---

## Roadmap courte

### MVP 0 (maintenant)

- Modèle de données : sites / services / backends
- Healthchecker
- Génération config Caddy + reload
- Multi-sites fonctionnel

### MVP 1

- API CRUD + auth token
- Tags/weights (ex: Dijon prioritaire)
- Observabilité (logs/metrics)

### MVP 2

- Agent par site
- Début de déploiement automatique (Portainer/Docker API)

---

## Critère de réussite Phase I

- Un service a au moins 2 backends sur 2 sites différents
- Tu coupes un site complet (VPN down, WAN down, machines off)
- Le service reste joignable via le domaine public
- Caddy bascule automatiquement vers le(s) site(s) restant(s)
