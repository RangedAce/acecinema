# HA-First Cluster Platform (from scratch)

## 🎯 Objectif du projet
Ce projet vise à construire from scratch une plateforme de type cluster HA, où la continuité de service est une propriété fondamentale du système.

L’objectif est de permettre :

- l’exécution de workloads sur plusieurs machines,
- la reprise automatique en cas de panne,
- la conservation de l’état applicatif,
- une coupure client nulle ou minimale.

La plateforme est pensée comme un socle générique, indépendant d’une application particulière.

## 🧠 Positionnement technique (choix assumés)
### Approche

- Implémentation from scratch (pas Kubernetes, pas Swarm).
- Architecture inspirée des systèmes distribués modernes (orchestrateurs, schedulers, control plane).
- Priorité à la compréhension, la maîtrise et l’évolutivité.

### Choix volontairement simplifié (Phase I)

- Une base de données centrale
- Accessible via un seul point logique
- Non redondée pour l’instant

👉 Ce choix est conscient et temporaire, afin de :

- réduire la complexité initiale,
- accélérer le développement du cœur du cluster,
- se concentrer sur le HA des workloads, pas encore sur le HA des données.

## 🏗️ Architecture globale
### 1. Control Plane (HA)

Le cluster repose sur un plan de contrôle chargé de :

- maintenir l’état global du cluster,
- connaître les nœuds disponibles,
- décider où lancer les workloads,
- détecter les pannes.

Caractéristiques :

- plusieurs instances possibles
- consensus / quorum prévu
- aucune instance unique critique à terme

### 2. Workers

Les workers sont des machines d’exécution :

- ils reçoivent des ordres du control plane,
- exécutent les workloads (conteneurs/process),
- remontent leur état (heartbeat, santé, charge).

Un worker peut tomber sans interrompre le service global.

### 3. Base de données (Phase I)

La base de données est utilisée pour :

-  l’état du cluster (métadonnées),
- l’état applicatif (config, sessions, jobs, etc.).

Caractéristiques actuelles :

- un seul endpoint
- pas de réplication
- SPOF assumé

Limitation connue :

```
Si la base tombe, le cluster ne peut plus évoluer,
mais les workloads déjà lancés peuvent continuer à tourner.
```
La redondance de la base est explicitement reportée à une phase ultérieure.

### 🔁 Gestion du cycle de vie des workloads

Le système fonctionne par intention :

- l’utilisateur définit un desired state
- le cluster s’assure que l’état réel converge vers cet objectif
- toute divergence (panne, crash, perte de nœud) déclenche une correction automatique

### 🌐 Routage et continuité client

- Les clients se connectent à un endpoint stable
- Le routage interne est dynamique
- Les instances défaillantes sont retirées automatiquement

Objectif :

- aucune configuration client à modifier
- reconnexion éventuelle, mais rapide et transparente

### 📦 Gestion de l’état applicatif

Principe clé :

- Aucun état critique ne doit être stocké localement sur un worker

Phase I :

- état centralisé en base unique
- accès contrôlé par le cluster

Phase II (future) :

- réplication
- leader election
- bascule automatique
- suppression du SPOF base de données

### 🚧 Limites actuelles (connues et acceptées)

- La base de données est un point unique de défaillance
- Le projet ne vise pas encore :
  - le multi-DC
  - la tolérance totale aux partitions réseau
- L’objectif est la stabilité fonctionnelle, pas la perfection théorique

## 🛣️ Roadmap simplifiée
### Phase I — Fondation

- cluster from scratch
- control plane fonctionnel
- workers + scheduling
- base centrale unique
- HA des workloads

### Phase II — Robustesse

- réplication de la base
- leader election DB
- tolérance aux pannes de données
- réduction drastique du SPOF

### Phase III — Maturité

- rolling updates
- autoscaling
- observabilité avancée
- politiques HA par défaut

### 🧪 Critère de réussite Phase I

Le projet est considéré valide si :

- un service tourne sur plusieurs workers
- un worker est coupé brutalement
- le service est relancé ailleurs automatiquement
- le client continue à accéder au service
- l’état applicatif est conservé (tant que la DB est disponible)

### 🧩 Vision

Ce projet n’essaie pas de battre les solutions existantes.
Il vise à comprendre, maîtriser et reconstruire les fondations d’un système HA moderne.

```
La haute disponibilité n’est pas un add-on.
C’est une propriété structurelle du système.
```