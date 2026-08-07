---
theme: default
title: Chaque namespace mérite ses backups
titleTemplate: '%s — Crystal Backup'
info: |
  Crystal Backup — backup Kubernetes multi-tenant, self-service, sans lock-in.
  Meetup 45 min (30 min slides + 12 min démo). https://github.com/CrystalBackup/CrystalBackup
author: Alexis Ducastel
colorSchema: dark
highlighter: shiki
lineNumbers: false
transition: slide-left
mdc: true
fonts:
  sans: Inter
  mono: JetBrains Mono
aspectRatio: 16/9
favicon: /crystal.svg
layout: center
class: text-center
---

<img src="/crystal.svg" class="h-36 mx-auto mb-6" style="filter: drop-shadow(0 24px 60px rgba(43,99,240,0.55));" alt="Crystal Backup" />

# Chaque namespace mérite <span class="grad">ses backups</span>

<p class="!text-lg">Backup Kubernetes multi-tenant, self-service, sans lock-in — et testé pour de vrai.</p>

<div class="mt-6 flex gap-2 justify-center">
  <span class="pill">Apache-2.0</span>
  <span class="pill">restic-compatible</span>
  <span class="pill">v0.6.3 · M0–M6 livrés</span>
</div>

<p class="tiny muted mt-10">Meetup — 45 min · slides + démo live</p>

<!--
[00:00 → 00:30]
Bonsoir ! 45 minutes : ~30 de slides, ~12 de démo live, et on garde de quoi respirer.
Une promesse d'entrée : zéro slide marketing. Tout ce que je vais affirmer est soit du code
que vous pouvez lire, soit un rapport de test publié — y compris les bugs.
-->

---
layout: two-cols
---

# `whoami`

<div class="panel mt-4">

**Alexis Ducastel**

- Fondateur **infraBuilder**
- Plateformes Kubernetes managées, multi-tenant
- Auteur de **Crystal Backup** (open source, Apache-2.0)

</div>

<div class="mt-4 tiny muted">
alexis@infrabuilder.com · github.com/CrystalBackup
</div>

::right::

<div class="center-abs">
<div class="panel accent" style="max-width: 320px">
<span class="eyebrow">disclaimer honnête</span>

Ce projet est **jeune** (v0.6.3), écrit **avec assistance IA** sous direction humaine,
et testé sur de vrais clusters **parce que** personne ne devrait me croire sur parole.

</div>
</div>

<!--
[00:30 → 01:30]
Qui je suis, en 20 secondes. Et tout de suite le cadre d'honnêteté : projet jeune,
construit avec des agents IA sous revue humaine — j'y reviendrai, c'est un des sujets
intéressants du projet — et c'est exactement pour ça que la partie « preuve » de ce talk
existe. Adaptez cette slide à votre bio du jour.
-->

---

<span class="eyebrow">un mardi matin ordinaire</span>

# Il est 9h04.

<v-clicks>

<div class="panel mt-4">💥 Un tenant vient de <code>DROP TABLE</code> sa prod. Il ouvre un ticket.</div>

<div class="panel mt-3">🎫 <em>« Bonjour, vous auriez un backup de mon namespace ? »</em></div>

<div class="panel problem mt-3">🤷 L'admin : « Oui… enfin, je crois. D'hier soir. Peut-être. Je regarde et je reviens vers vous. »</div>

<div class="mt-5 qbig">Le tenant ne peut ni <span class="grad">vérifier</span>, ni <span class="grad">déclencher</span>, ni <span class="grad">restaurer</span>. Il peut <em>attendre</em>.</div>

</v-clicks>

<!--
[01:30 → 03:00]
Scène vécue par toutes les équipes plateforme. Le point n'est pas que le backup n'existe
pas — il existe souvent ! Le point est que le TENANT n'a aucune prise dessus.
Laisser un blanc après la dernière phrase, elle porte tout l'acte 1.
-->

---

<span class="eyebrow">le décor</span>

# La plateforme multi-tenant typique

<div class="chips mt-8 justify-center">
  <div class="chip accent">cluster K8s managé</div>
  <div class="sep"></div>
  <div class="chip">ns: team-a</div>
  <div class="chip">ns: team-b</div>
  <div class="chip">ns: team-c</div>
  <div class="sep"></div>
  <div class="chip repo">Velero · daily · admin-only</div>
</div>

<v-click>

<div class="mt-8 qbig text-center">
Le cluster se sauvegarde lui-même.<br/><span class="grad">Ses tenants, eux, ne peuvent pas.</span>
</div>

</v-click>

<v-click>

<p class="text-center mt-4">L'outil cluster-wide est un <strong>filet de sécurité admin</strong> — c'est très bien, gardez-le.<br/>Mais il ne donne <strong>rien</strong> aux tenants.</p>

</v-click>

<!--
[03:00 → 04:30]
Isolation par namespace + RBAC, un outil de backup cluster-wide qui tourne la nuit,
rétention courte, piloté par l'admin. Configuration ultra-classique, et saine !
La phrase-clé traduit le hero du site : « Managed clusters back themselves up.
Their tenants can't. »
-->

---

<span class="eyebrow">côté tenant</span>

# Trois frustrations, toujours les mêmes

<div class="grid grid-cols-3 gap-4 mt-8">

<v-click>
<div class="panel problem">
<div class="text-3xl mb-2">🚫</div>

**Aucune action**

Pas de backup à la demande, pas de restore self-service. Tout passe par un ticket.
</div>
</v-click>

<v-click>
<div class="panel problem">
<div class="text-3xl mb-2">🙈</div>

**Aucune visibilité**

Suis-je sauvegardé ? Quoi ? Depuis quand ? <em>Mystère.</em>
</div>
</v-click>

<v-click>
<div class="panel problem">
<div class="text-3xl mb-2">🔒</div>

**Aucune sortie**

Impossible de copier <em>hors</em> plateforme, sous sa propre clé, hors de la confiance de l'opérateur.
</div>
</v-click>

</div>

<v-click>

<p class="text-center mt-8 muted">Trois manques, une cause : le backup est un service <strong>rendu à l'admin</strong>, pas <strong>aux tenants</strong>.</p>

</v-click>

<!--
[04:30 → 05:30]
Ces trois frustrations viennent mot pour mot de la spec du projet (spec/00-requirements).
La troisième est la plus sous-estimée : même avec un admin parfait, certains tenants ont
des obligations (réglementaires, contractuelles) de détenir une copie sous LEUR clé.
-->

---
layout: center
---

<div class="drake" style="max-width: 620px">
  <div class="face">🙅</div>
  <div class="say no">Ouvrir un ticket, attendre 3 jours, espérer que le backup d'avant-hier contient la table</div>
  <div class="face">👉😎</div>
  <div class="say yes"><code>kubectl apply -f restore.yaml</code></div>
</div>

<!--
[05:30 → 06:00]
Respiration meme. (Format « Drake » redessiné en CSS — aucun copyright froissé.)
Le self-service n'est pas un luxe UX : c'est la différence entre un RTO en minutes
et un RTO en réunions.
-->

---

<span class="eyebrow">l'existant</span>

# De très bons outils… pour d'autres problèmes

<div class="mt-4"></div>

| | <span class="mark-ok">Crystal Backup</span> | Velero | K8up | VolSync | Kasten K10 |
|---|:--:|:--:|:--:|:--:|:--:|
| Self-service tenant | <span class="mark-ok">✓</span> | <span class="mark-no">–</span> | <span class="mark-ok">✓</span> | <span class="mark-mid">~</span> | <span class="mark-mid">~</span> |
| Isolation par namespace | <span class="mark-ok">✓</span> | <span class="mark-no">–</span> | <span class="mark-mid">~</span> | <span class="mark-mid">~</span> | <span class="mark-mid">~</span> |
| Copie off-platform, clé du tenant | <span class="mark-ok">✓</span> | <span class="mark-mid">~</span> | <span class="mark-ok">✓</span> | <span class="mark-ok">✓</span> | <span class="mark-no">–</span> |
| Données PVC **+** manifests | <span class="mark-ok">✓</span> | <span class="mark-ok">✓</span> | <span class="mark-no">–</span> | <span class="mark-no">–</span> | <span class="mark-ok">✓</span> |
| DR depuis le dépôt seul | <span class="mark-ok">✓</span> | <span class="mark-mid">~</span> | <span class="mark-no">–</span> | <span class="mark-no">–</span> | <span class="mark-mid">~</span> |
| Relisible avec un outil standard | <span class="mark-ok">✓ restic</span> | <span class="mark-mid">~</span> | <span class="mark-ok">✓</span> | <span class="mark-ok">✓</span> | <span class="mark-no">–</span> |

<p class="tiny muted mt-3">Colonne Crystal Backup = v0.6.3 livrée, chaque ✓ exercé par un rapport d'acceptation publié. Les autres colonnes : à vérifier contre leurs docs — capacités mouvantes, objectifs différents. Ceci n'est <strong>pas</strong> un benchmark.</p>

<!--
[06:00 → 07:30]
Dire du bien des autres outils, sincèrement : Velero est excellent en DR admin,
K8up/VolSync excellents en restic/réplication par namespace, Kasten très riche mais
propriétaire. Le point : AUCUN ne couvre la COMBINAISON de la colonne de gauche.
Chacun résout une partie du problème ; les trous diffèrent.
-->

---
layout: center
class: text-center
---

<span class="eyebrow">le trou dans la raquette</span>

<div class="qbig" style="font-size: 1.9rem; max-width: 800px">
La combinaison manquante :<br/>
<span class="grad">self-service multi-tenant</span> + <span class="grad">réversibilité</span> + <span class="grad">DR depuis le dépôt</span>
</div>

<p class="mt-8 muted">C'est exactement — et seulement — ce trou que Crystal Backup vient boucher.<br/><strong>En coexistant</strong> avec l'outil déjà en place, pas en le remplaçant.</p>

<!--
[07:30 → 08:00]
Transition d'acte. Insister sur « coexistence, pas remplacement » — on y reviendra :
personne n'a envie d'un projet « rip and replace » pour ses backups.
-->

---
layout: section
---

<span class="eyebrow">acte 2</span>

# La <span class="grad">solution</span>

<p class="mt-4">Un opérateur, deux plans, des dépôts restic standards.</p>

<!--
[08:00 → 08:30]
Annonce du plan de l'acte 2 : le modèle deux plans, la cascade, le stockage,
puis les deux sujets qui fâchent — l'isolation et les clés.
-->

---

<span class="eyebrow">le modèle</span>

# Deux plans, façon cert-manager

<div class="grid grid-cols-2 gap-5 mt-6">

<v-click>
<div class="panel accent">
<span class="pill grad-fill mb-2">plan cluster · admin</span>

**Le DR de la plateforme.** Tous les namespaces (ou une sélection) → **un dépôt restic partagé**, tenancy portée par les **tags** (`tenant=`, `namespace=`, `pvc=`).

<div class="tiny muted mt-2"><code>ClusterBackupLocation</code> · <code>ClusterBackupSchedule</code> · <code>ClusterBackup</code> · <code>ClusterRestore</code> · <code>ClusterErasure</code> · <code>ClusterBackupExternalSync</code></div>
</div>
</v-click>

<v-click>
<div class="panel accent">
<span class="pill mb-2">plan namespace · tenant</span>

**Le self-service, en plus.** Chaque tenant peut sauvegarder son namespace vers **son bucket, ses credentials, sa clé** — que la plateforme ne possède pas.

<div class="tiny muted mt-2"><code>BackupLocation</code> · <code>BackupSchedule</code> · <code>Backup</code> · <code>Restore</code> · <code>BackupExternalSync</code></div>
</div>
</v-click>

</div>

<v-click>

<p class="text-center mt-6">« L'admin protège tout le monde dans un dépôt mutualisé ;<br/>le tenant peut <strong>en plus</strong> s'auto-protéger chez lui, avec une clé que la plateforme n'a pas. »</p>

</v-click>

<!--
[08:30 → 10:30]
L'analogie qui parle à tout le monde : ClusterIssuer vs Issuer de cert-manager.
C'est un ET, pas un OU : le plan namespace s'AJOUTE au DR cluster.
Détail élégant : le layout de snapshot est IDENTIQUE sur les deux plans → un seul code
mover/restore, et restic upstream lit les deux. 12 CRDs au total (11 publics + 1 interne).
-->

---

<span class="eyebrow">la mécanique</span>

# La cascade — comme CronJob → Job → Pod

<div class="mt-10 chips justify-center">
  <v-click><div class="chip accent">ClusterBackupSchedule<small>≈ CronJob</small></div></v-click>
  <v-click><div class="sep"></div><div class="chip">ClusterBackup<small>≈ un run</small></div></v-click>
  <v-click><div class="sep"></div><div class="chip">Backup × N<small>1 par namespace</small></div></v-click>
  <v-click><div class="sep"></div><div class="chip">mover Job × M<small>1 par PVC</small></div></v-click>
  <v-click><div class="sep"></div><div class="chip repo">dépôt restic<small>s3://bucket/prefix/clusterID</small></div></v-click>
</div>

<v-click>

<div class="grid grid-cols-3 gap-3 mt-10 text-sm">
<div class="panel">🔗 Enfants liés par <strong>label</strong>, pas ownerReference : purger l'historique ne supprime <strong>jamais</strong> un backup restaurable.</div>
<div class="panel">📦 Le <code>Backup</code> namespacé est <strong>l'unité d'exécution unique</strong> des deux plans — et le point de restauration.</div>
<div class="panel">🛃 Movers <strong>non privilégiés</strong>, uniquement dans <code>crystal-backup-system</code> — jamais un pod de backup chez un tenant.</div>
</div>

</v-click>

<!--
[10:30 → 12:00]
Analogie CronJob → Job → Pod : tout le monde suit immédiatement.
Le fan-out : un run résout les namespaces (globs, labels), crée UN Backup par namespace,
qui crée UN mover Job par PVC — répartis sur les nœuds pour agréger la bande passante.
Le choix label-pas-ownerReference est le genre de détail qui montre que le design pense
« données » avant « objets K8s ».
-->

---

<span class="eyebrow">dans un backup</span>

# Huit étapes, deux détails qui changent tout

```text
resolve → hooks pre → snapshot CSI → hooks post → expose → mover restic → manifests → cleanup
```

<div class="grid grid-cols-2 gap-4 mt-6">

<v-click>
<div class="panel accent">

**La fenêtre de gel est bornée par le snapshot, pas par l'upload.**

Les hooks `post` partent dès que les snapshots sont **coupés** — pas quand l'upload finit.

<p class="tiny muted mt-1">« Une base gelée pendant un upload de plusieurs heures, c'est un outage, pas un backup. »</p>
</div>
</v-click>

<v-click>
<div class="panel accent">

**Un CSI sans snapshot → `Skipped` + raison, dans le status.**

Pas de fallback qui lit le volume live, pas d'oubli silencieux. <strong>« Jamais un drop silencieux. »</strong>

<p class="tiny muted mt-1">Backup depuis un snapshot read-only, ou pas de backup du volume — et c'est écrit.</p>
</div>
</v-click>

</div>

<v-click>

<p class="mt-5 text-center muted">Et le dégel est <strong>inconditionnel</strong> — même si un snapshot a échoué. Votre base ne reste pas gelée parce qu'un backup a raté.</p>

</v-click>

<!--
[12:00 → 13:30]
Pas le temps de détailler les 8 étapes — le spec le fait très bien. Deux choix de design
à retenir. Si vous êtes en retard sur l'horaire : cette slide se résume en 30 secondes
avec les deux titres en gras.
-->

---

<span class="eyebrow">le mouvement de données</span>

# Least data movement — Ceph-aware

<div class="mt-6 grid grid-cols-3 gap-4">

<div class="panel">
<span class="pill mb-2">CephFS</span>

**Shallow volume** : montage ROX sur le `backingSnapshot`. **Zéro copie** avant l'upload.
</div>

<div class="panel">
<span class="pill mb-2">RBD & CSI génériques</span>

Clone **copy-on-write** depuis le `VolumeSnapshot` — on ne déplace que ce qu'on lit.
</div>

<div class="panel">
<span class="pill mb-2">dédup restic</span>

Content-defined chunking + zstd : d'un run à l'autre, seuls les blocs **nouveaux** partent vers S3.
</div>

</div>

<v-click>

<div class="panel mt-6">
📖 Les backups sont pris depuis un <strong>snapshot read-only</strong>, monté par un Job éphémère — l'opérateur lui-même <strong>ne touche jamais un octet de données</strong>.
</div>

</v-click>

<!--
[13:30 → 15:00]
Pour un public storage : le chemin CephFS shallow est le grand gagnant — zéro copie locale.
Pour les autres CSI, chemin générique snapshot → PVC COW temporaire.
L'opérateur orchestre ; ce sont des Jobs jetables non privilégiés qui font le restic.
-->

---

<span class="eyebrow">l'inversion clé</span>

# Le dépôt est la source de vérité

<div class="grid grid-cols-2 gap-5 mt-5">

<div>

<v-clicks>

- Les CRs `Backup` sont une **projection** du dépôt restic — pas l'inverse
- Un contrôleur de **discovery** inventorie le dépôt et projette les `Backup` dans les namespaces
- Durée de vie du CR **=** durée de vie de la donnée

</v-clicks>

</div>

<div>

<v-click>

```console
$ kubectl get backups -n team-a
NAME                    PHASE      LOCATION    BACKUP-TIME  AGE
dr-daily-20260731-0200  Completed  dr-primary  1d           1d
dr-daily-20260801-0200  Completed  dr-primary  2h           2h
```

<p class="tiny mt-2 text-center"><strong>Exactement</strong> ce qui est restaurable.<br/>Rien de plus, rien de moins.</p>

</v-click>

</div>

</div>

<v-click>

<div class="panel accent mt-5">
💡 Conséquence : etcd peut brûler, les CRs peuvent disparaître — <strong>tant que le bucket existe, tout est là.</strong>
</div>

</v-click>

<!--
[15:00 → 16:30]
LE choix d'architecture du projet (R26). La plupart des outils font l'inverse : le CR est
la vérité et le storage suit — et le jour où etcd meurt, l'inventaire meurt avec.
Ici `kubectl get backups` est une VUE MATÉRIALISÉE du dépôt.
-->

---

<span class="eyebrow">le pire jour de votre carrière</span>

# DR : un cluster neuf, un bucket, une clé

<div class="mt-6">

```yaml{1-4|all}
# Cluster tout neuf : 2 Secrets (KEK + creds S3), puis UNE ressource retrouve tout
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata: { name: dr-primary }
spec:
  clusterID: prod-eu-1          # ← pointe sur le dépôt existant
  s3:
    endpoint: …
    bucket: …
    prefix: …
    credentialsSecretRef: { name: dr-s3 }
  encryption: { clusterKEKSecretRef: { name: cluster-kek } }
```

</div>

<v-click>

<div class="chips mt-5 justify-center">
  <div class="chip">bucket S3 existant</div>
  <div class="sep"></div>
  <div class="chip accent">discovery inventorie</div>
  <div class="sep"></div>
  <div class="chip"><code>kubectl get backups</code> se remplit</div>
  <div class="sep"></div>
  <div class="chip repo">ClusterRestore</div>
</div>

<p class="text-center mt-4 muted"><strong>Aucun CR préexistant. Aucun cluster survivant.</strong> La DEK wrappée est même escrowée <em>dans le bucket</em> — il ne manque que la KEK, gardée hors cluster.</p>

</v-click>

<!--
[16:30 → 17:30]
Le scénario disaster-recovery-first : discovery = pointer l'opérateur sur un bucket et il
liste ce qui est restaurable. Un ClusterRestore cible une COORDONNÉE DE DÉPÔT
(location + namespace + run), pas un objet — il peut recréer le namespace disparu.
Teaser : c'est exactement ce qu'on fait dans la démo.
-->

---

<span class="eyebrow">anti lock-in</span>

# La réversibilité est un format, pas une promesse

<div class="grid grid-cols-2 gap-5 mt-5">

<div class="panel">

**Des dépôts restic standards.** Pas de catalogue propriétaire, pas de format maison, pas de couche de chiffrement custom.

<p class="tiny muted mt-2">restic repo v2 — zstd, dédup, AES-256. Vos données, lisibles par un outil que <em>nous</em> ne shippons pas.</p>

</div>

<div>

```console
$ export RESTIC_PASSWORD=…   # votre clé
$ restic -r s3:https://s3.…/bucket/dr/prod-eu-1 snapshots
ID        Host       Tags                        Paths
a1b2c3d4  prod-eu-1  namespace=demo,pvc=data,…   /data/demo/data
```

<p class="tiny mt-2 text-center">Aucun composant Crystal Backup dans cette commande.</p>

</div>

</div>

<v-click>

<div class="qbig text-center mt-6">
Le jour où ce projet vous déçoit,<br/><span class="grad">vous partez avec vos données.</span>
</div>

</v-click>

<!--
[17:30 → 18:30]
L'argument anti-lock-in le plus fort : la garantie de sortie n'est pas une feature,
c'est le FORMAT. C'est aussi pour ça que le CLI n'est pas pressé — et pour ça qu'il peut
sortir du dépôt : restic upstream EST déjà le CLI de secours. On le démontre en live dans
la démo.
-->

---

<span class="eyebrow">le sujet qui fâche n°1</span>

# Une isolation qui tient sans bonne volonté

<div class="grid grid-cols-2 gap-4 mt-5">

<v-click>
<div class="panel accent">

**Le champ n'existe pas.**

Un `Restore` namespacé n'a **ni** `locationRef`, **ni** target-namespace. Le filtre `namespace=<ns du CR>` est dérivé **côté serveur** — non forgeable, jamais déclaré par l'utilisateur.

</div>
</v-click>

<v-click>
<div class="panel accent">

**Les movers restent chez l'opérateur.**

Jamais un pod de backup, une clé ou un Secret plateforme dans un namespace tenant. Data movers : **zéro token ServiceAccount**, egress S3 uniquement.

</div>
</v-click>

</div>

<v-click>

<div class="mt-6 panel">
🧱 « <strong>Admission is a gate, not the isolation boundary.</strong> » Les contrôleurs re-dérivent le filtre à l'exécution : une VAP bypassée dégrade l'UX, jamais la tenancy. <em>Aucune couche unique ne porte l'isolation.</em>
</div>

</v-click>

<!--
[18:30 → 20:00]
Le principe : « Isolation is enforced server-side, never client-declared. »
La forme de l'API EST la sécurité : on ne valide pas un champ dangereux, on ne le crée pas.
Un nom de run d'un autre namespace ne « fuite » pas : il ne RÉSOUT simplement pas.
En démo : on essaiera de tricher en vrai.
-->

---

<span class="eyebrow">le sujet qui fâche n°2 — les clés</span>

# « Une clé née dans le cluster meurt avec lui »

<div class="chips mt-8 justify-center">
  <div class="chip accent">KEK — générée par l'admin<small>escrowée HORS cluster, jamais créée par l'opérateur</small></div>
  <div class="sep"></div>
  <div class="chip">DEK du dépôt, wrappée (age)<small>Secret + escrowée dans le bucket</small></div>
  <div class="sep"></div>
  <div class="chip repo">chiffrement restic natif<small>AES-256, format standard</small></div>
</div>

<v-clicks>

<div class="mt-8 grid grid-cols-2 gap-4">
<div class="panel">🔑 Ni l'opérateur ni le chart Helm ne <strong>créeront jamais</strong> la KEK. Absente → la location se met en dégradé <code>KEKMissing</code>, bruyamment.</div>
<div class="panel">🌪️ Perte totale du cluster : KEK (hors cluster) + bucket (DEK escrowée) = <strong>tout se retrouve</strong>. Rotation de KEK : re-wrap, zéro mouvement de données.</div>
</div>

</v-clicks>

<!--
[20:00 → 21:00]
L'anti-pattern classique : l'outil de backup qui génère sa clé DANS le cluster —
le jour où le cluster meurt, la clé meurt avec, et tous les backups deviennent des
déchets chiffrés. Ici la custody de la racine reste volontairement dans les mains de
l'admin, hors cluster. C'est une contrainte d'install assumée, pas un oubli d'UX.
-->

---
layout: center
---

<span class="eyebrow">et côté tenant ?</span>

<div class="qbig text-center" style="max-width: 820px">
« La garantie s'achète par <span class="grad">l'absence du mécanisme</span> —<br/>aucun flag, bypass ou futur mainteneur ne peut l'éteindre. »
</div>

<div class="panel mt-8" style="max-width: 700px">

Sur le dépôt d'un tenant, la plateforme n'a **aucun slot de clé** — et **aucun champ d'API pour en demander un**. La feature opt-in `platformAccess` a été **supprimée plutôt qu'implémentée** : un slot restic aurait survécu à la rotation de clé du tenant. *« Opting in was a one-way door they were not told about. »* (ADR 0004)

</div>

<!--
[21:00 → 22:00]
Ma décision préférée du projet. Pour aider un tenant, un admin doit lire le Secret DANS le
namespace du tenant : visible dans l'audit log, et ça cesse de marcher dès que le tenant
rotate sa clé. La révocabilité est tout le point. La sécurité par structure, pas par policy.
-->

---

<span class="eyebrow">rgpd</span>

# Le droit à l'effacement — physique

<div class="grid grid-cols-2 gap-5 mt-5">

<div>

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterErasure
metadata: { name: bye-team-x }
spec:
  locationRef: { name: dr-primary }
  target: { namespace: team-x }     # tenant / ns / PVC
  confirmation: team-x              # R23 — nom exact, vérifié par CEL
```

</div>

<div>

<v-clicks>

- `restic forget --tag namespace=team-x` + `prune` : **suppression physique** dans le dépôt partagé
- Trois granularités : tenant, namespace, PVC
- Honnêteté : **pas de crypto-shredding par tenant** — « dans un dépôt à clé unique, des DEK par namespace seraient une fiction »
- Tension assumée : sur une location immutable (Object Lock, M9), l'effacement **attend l'expiry** — WORM vs RGPD, arbitré explicitement

</v-clicks>

</div>

</div>

<!--
[22:00 → 23:00]
Le "droit à l'oubli" en pratique : physique, taggé, confirmé (le champ confirmation doit
nommer exactement la cible — vérifié par ValidatingAdmissionPolicy dans l'API server).
La posture : plutôt une vraie suppression physique qu'un crypto-shredding qu'on ne peut
pas garantir. Et le conflit immutabilité/effacement est documenté, pas caché.
-->

---
layout: center
class: text-center
---

<span class="eyebrow">déploiement</span>

# Coexistence, <span class="grad">pas remplacement</span>

<p class="mt-4" style="max-width: 720px">API group, namespace, credentials, dépôts et objets snapshot <strong>distincts</strong>.<br/>Crystal Backup tourne <strong>à côté</strong> de Velero (ou autre) sur les mêmes PVCs.</p>

<div class="mt-6 pill grad-fill">« L'adopter n'exige jamais d'enlever un autre outil. »</div>

<p class="tiny muted mt-6">Gardez votre filet de sécurité actuel. Ajoutez le self-service. Comparez. Décidez plus tard.</p>

<!--
[23:00 → 23:30]
Point d'adoption crucial : personne ne remplace son outil de backup sur un coup de tête,
et le projet ne le demande PAS. La coexistence n'est plus un milestone parce qu'elle est
structurelle et déjà livrée — groupe d'API distinct, aucune mutation des classes de
snapshot d'autrui, et une alerte qui compte les snapshots de TOUS les outils, parce qu'en
coexistence ce sont ceux de l'autre qui remplissent la réserve partagée par image RBD. Ce
qui reste dû, c'est le soak de M6 — pas encore fait, et dit comme tel.
-->

---
layout: section
---

<span class="eyebrow">acte 3</span>

# La <span class="grad">preuve</span>

<p class="mt-4 qbig">« Backup software has one unforgivable failure mode:<br/>a restore that does not work. »</p>

<!--
[23:30 → 24:00]
Changement de ton. Tout ce que je viens de raconter, pourquoi me croire ?
Réponse : ne me croyez pas. Voici comment le projet se teste, et voici ses bugs.
-->

---

<span class="eyebrow">le banc d'essai</span>

# Le crucible — un vrai cluster, jetable

<div class="grid grid-cols-2 gap-5 mt-5">

<div>

<v-clicks>

- **RKE2 HA + Rook-Ceph (RBD + CephFS) + Longhorn + local-path** sur Hetzner Cloud — *created, used, destroyed*
- 6 namespaces tenants archétypaux : StatefulSets checksummés, RWX partagé, données exotiques (sparse, hardlinks, xattrs, unicode), PVC détachés, **et un tenant sans snapshots** — pour tester le refus honnête
- Chaque volume est seedé avec un `MANIFEST.sha256` — l'étalon byte-for-byte des restores

</v-clicks>

</div>

<div>

<v-click>

<div class="panel accent">
<span class="eyebrow">le prix de la vérité</span>

**≈ 1 € les 2 heures** de validation sur infra réelle.

<p class="tiny muted mt-2">~0,52 €/h le cluster complet. Jamais en CI de PR — à la demande, détruit à la fin.</p>
</div>

<div class="panel mt-4">
📜 Chaque milestone est accepté là-dessus <strong>avant</strong> de sortir. Les rapports sont publiés — les 90 checks, les durées, les skips, <strong>et les défauts trouvés</strong>.

<p class="tiny muted mt-2">Dernier : <code>…/reports/crucible-m6-3.html</code> — 90/90, 0 skip, 2 h 43.</p>
</div>

</v-click>

</div>

</div>

<!--
[24:00 → 25:30]
kind et envtest ne suffisent pas pour un outil de backup : les bugs intéressants vivent
dans les vrais CSI, le vrai S3, les vraies races. D'où une plateforme réelle, jetable,
à ~1€ les 2h — moins cher qu'un café parisien, et infiniment plus honnête qu'un mock.
-->

---

<span class="eyebrow">l'idée centrale</span>

# L'oracle restic indépendant

<div class="chips mt-8 justify-center">
  <div class="chip">l'opérateur écrit le dépôt</div>
  <div class="sep"></div>
  <div class="chip accent">un Job jetable, restic <strong>upstream</strong> épinglé</div>
  <div class="sep"></div>
  <div class="chip repo">relit, dump, compare byte à byte</div>
</div>

<v-click>

<div class="qbig text-center mt-10">
Le contrôleur ne corrige pas <span class="grad">sa propre copie</span>.
</div>

<p class="text-center mt-4 muted">Un bug systématique s'annulerait des deux côtés si le même code écrivait <em>et</em> relisait.<br/>L'oracle utilise un outil que le projet ne ship pas — et depuis M1, un scénario refait le restore<br/><strong>entièrement hors cluster</strong>, avec le seul binaire restic et le mot de passe.</p>

</v-click>

<v-click>

<p class="text-center tiny mt-4">« This gate is what makes R8 a fact rather than a slogan. »</p>

</v-click>

<!--
[25:30 → 26:30]
L'idée à voler pour VOS projets : ne jamais laisser un système se noter lui-même.
Même logique pour la sync externe : le dépôt destination doit s'ouvrir avec SA clé
et REFUSER la clé source — sinon le re-chiffrement est une croyance, pas un fait.
-->

---

<span class="eyebrow">war story n°1</span>

# « On signait la supply chain avec `head -n1` »

<div class="warbox mt-5">

**Pendant 4 releases**, `cosign verify` échouait pour **tout consommateur** : la signature, le SBOM et la provenance SLSA étaient accrochés au **child manifest amd64** — pas à l'index multi-arch que le tag résout.

<v-clicks>

- Le pipeline : **vert**. Le commentaire dans le code : *« c'est l'index »* — **faux**. La propre doc du projet mettait en garde contre **exactement** ce construct.
- Trouvé en vérifiant **les artefacts** de la release, pas les ticks verts du pipeline. Personne n'avait jamais lancé `cosign verify` de l'extérieur.
- Fix : le job résout l'index depuis le registry et **refuse de signer autre chose qu'un index**.

</v-clicks>

</div>

<v-click>

<div class="pill grad-fill mt-5">Morale : vérifiez les artefacts, pas les pipelines.</div>

</v-click>

<!--
[26:30 → 27:30]
Le mensonge à trois étages : pipeline vert + commentaire faux + doc qui savait.
Publié tel quel dans le changelog 0.5.1, avec le titre « the signed artefact was the
wrong one ». Si vous signez des images multi-arch : allez vérifier DEMAIN MATIN que
votre signature est sur l'index, pas sur l'enfant amd64.
-->

---

<span class="eyebrow">war story n°2</span>

# Le backup qui dit `Completed`… sans rien écrire

<div class="warbox mt-5">

Même `ClusterBackup` relancé 4 fois, volume re-seedé entre chaque run : **un seul snapshot existe**. Les 3 runs suivants rapportent `Completed, pvcsSucceeded=1` — en comptant **les bytes d'un autre run** comme les leurs.

<v-clicks>

- Cause : l'existence de l'enfant testée sur `(namespace, name)` seulement — une projection de discovery ou un vieux `Backup` homonyme satisfaisait le test. Le contrôleur no-opait, content.
- Le bug datait de **M1**. Trouvé par le **gate de fidélité de restauration** de M6… **à son premier run réel.**
- Fix : identité par `UID` du parent ; toute discordance = `RunNameCollision`, échec **bruyant**.

</v-clicks>

</div>

<v-click>

<p class="mt-4 text-center">Le seul signal honnête était <code>addedBytes: 0</code> — <strong>et personne n'alerte là-dessus.</strong><br/><span class="muted tiny">La pire classe de défaut d'un outil de backup : le succès qui ment.</span></p>

</v-click>

<!--
[27:30 → 28:30]
LA war story. Rien n'était corrompu — rien n'était ÉCRIT. `restic check` : parfait.
C'est l'argument entier pour construire des gates de fidélité : il a payé à son premier run.
Au passage : c'est aussi pour ça que la démo tout à l'heure timestampe ses noms de run.
-->

---

<span class="eyebrow">war story n°3</span>

# Le droit à l'oubli qui n'oubliait rien

<div class="warbox mt-5">

L'écriture de la suite d'acceptation M5 a trouvé **3 features annoncées, documentées, testées — et complètement inertes** sur infra réelle :

<v-clicks>

- la sync externe tenant ne pouvait **jamais** atteindre `Completed` ;
- `mode: Mirror` n'a jamais rien pruné — son Job partait **sans image de conteneur** ;
- l'effacement RGPD n'a **jamais supprimé un snapshot** : `restic forget` sans keep-policy refuse poliment (exit 1) — et rien ne l'avait jamais exécuté contre un vrai dépôt.

</v-clicks>

</div>

<v-click>

<div class="qbig text-center mt-5">« The visible step succeeded<br/>and <span class="grad">the step after it</span> failed. »</div>

<p class="tiny muted text-center mt-2">Pourquoi une CI verte + des unit tests ne voient pas une feature entièrement morte : aucune des trois n'était atteignable sans un vrai dépôt.</p>

</v-click>

<!--
[28:30 → 29:30]
Trois fois la même morale dans une seule release. Publié dans le changelog ET sur le site,
avec la phrase-pattern. Si vous devez couper une war story pour tenir l'horaire : celle-ci
se résume en une phrase depuis la slide suivante.
-->

---
layout: center
---

<span class="eyebrow">l'échelle de la confiance</span>

<div class="brain" style="max-width: 700px">
  <v-click><div class="lvl"><div class="ico">🧠</div><div><strong>Unit tests verts.</strong> <span class="muted">Le code fait ce que le code dit.</span></div></div></v-click>
  <v-click><div class="lvl l2"><div class="ico">🤯</div><div><strong>CI verte, e2e sur kind.</strong> <span class="muted">Les objets bougent dans un vrai API server.</span></div></div></v-click>
  <v-click><div class="lvl l3"><div class="ico">🌌</div><div><strong>Cluster réel, vrais CSI, vrai S3.</strong> <span class="muted">Les bugs intéressants habitent ici.</span></div></div></v-click>
  <v-click><div class="lvl l4"><div class="ico">🔮</div><div><strong>Un oracle indépendant relit tout, et les rapports sont publics — défauts inclus.</strong></div></div></v-click>
</div>

<v-click>

<p class="text-center mt-6 muted">Le thème récurrent des bugs ci-dessus : « <strong>an absence reading as health</strong> » —<br/>l'alerte qui ne peut pas sonner, le check qui s'auto-skippe, le skip qui se lit comme un pass.</p>

</v-click>

<!--
[29:30 → 30:30]
Format « galaxy brain » maison. Le fil rouge de toutes ces stories : une absence qui se lit
comme de la santé. L'antidote est structurel : des gates qui ne peuvent PAS s'auto-
désactiver — pas de flag, pas de Skip() conditionnel, pas de tolérance réglable.
-->

---

<span class="eyebrow">et l'éléphant dans la pièce</span>

# Construit avec l'IA — vérifié comme si c'était faux

<div class="grid grid-cols-2 gap-5 mt-6">

<div class="panel">

**Assumé, écrit en toutes lettres** : specs, ADRs et code produits avec une assistance IA importante, sous direction et revue humaines. C'est aussi une **expérience d'ingénierie logicielle assistée**, documentée en public.

</div>

<div class="panel accent">

**La conséquence logique** : ne pas croire l'auteur — humain ou IA — sur parole. D'où l'oracle, le crucible, les gates qui ne peuvent pas se taire, et les rapports publiés.

</div>

</div>

<v-click>

<div class="qbig text-center mt-8">20 ADRs publics. 90 checks publiés.<br/><span class="grad">Vous pouvez relire chaque décision.</span></div>

</v-click>

<!--
[30:30 → 31:30]
Ne pas esquiver le sujet : oui, il y a beaucoup d'IA dans ce projet, c'est écrit sur le
README et sur le site. La réponse sérieuse à « peut-on faire confiance à du code assisté
par IA ? » n'est pas « promis, on a relu » — c'est une chaîne de vérification qui ne
suppose la bonne foi de personne. Ça vaut d'ailleurs pour le code 100% humain.
-->

---
layout: section
---

<span class="eyebrow">acte 4</span>

# La <span class="grad">démo</span>

<div class="mt-6 chips justify-center">
  <div class="chip">backup d'un namespace</div>
  <div class="sep"></div>
  <div class="chip danger" style="border-color: rgba(255,122,138,0.5)">kubectl delete namespace 💀</div>
  <div class="sep"></div>
  <div class="chip">restauration depuis le dépôt</div>
  <div class="sep"></div>
  <div class="chip accent">lecture avec restic upstream</div>
  <div class="sep"></div>
  <div class="chip repo">tricher → refusé</div>
</div>

<p class="tiny muted text-center mt-8">kind + S3 in-cluster, 100 % local. Que peut-il bien se passer ? 🤞</p>

<!--
[31:30 → 43:00 — LA DÉMO, ~12 min]
Basculer sur le terminal. Le runbook minuté est dans promotion/demo/README.md :
1. seed + backup (3') → 2. delete namespace (1') → 3. ClusterRestore + checksum (3')
→ 4. restic upstream depuis le laptop (2') → 5. CR forgé + mauvaise confirmation
refusés par l'admission (2'). Garder cette slide affichée pendant qu'on bascule.
Plan B : replay enregistré (voir demo/README.md § Plan B).
-->

---

<span class="eyebrow">l'honnêteté jusqu'au bout</span>

# Ce que vous n'avez PAS vu (parce que ça n'existe pas encore)

<div class="grid grid-cols-3 gap-4 mt-6">

<div class="panel problem">
<span class="pill mb-2">M7</span>

**Le stockage sans snapshot**

Une PVC sur `local-path` ou NFS simple est **sautée** — tout un parc k3s/RKE2 sans rien.
</div>

<div class="panel problem">
<span class="pill mb-2">M8</span>

**Les drills de restauration**

Comparer un restore à sa source, on sait faire — mais **en CI, pas chez vous**.
</div>

<div class="panel problem">
<span class="pill mb-2">M9</span>

**Immutabilité (S3 Object Lock)**

Le champ `mode: Immutable` est accepté — **il ne donne pas de WORM aujourd'hui.**
</div>

</div>

<v-click>

<div class="panel danger mt-6">
⚠️ <strong>0.6.3 s'offre au test en conditions réelles, pas à la production.</strong> API <code>v1alpha1</code> — elle bougera encore. Testez sur un cluster dont vous pouvez encaisser la perte, <strong>à côté de</strong> vos backups actuels. Et testez vos restores — c'est la pratique sur laquelle ce projet tourne.
</div>

</v-click>

<!--
[43:00 → 44:00]
La slide qui rend tout le reste crédible. La doc a même une page « When NOT to use it ».
Roadmap : M7 la portée (stockage sans snapshot, restore fichier, webhooks), M8 la preuve
(drills + alertes restore), M9 Object Lock → 1.0.0 comme décision délibérée de stabilité
d'API, pas comme accident de compteur.
Si on demande la CLI et l'UI : elles sortent du dépôt — plugin krew et projet séparé. Rien
n'est perdu, le repo est du restic standard, et aucune capacité ne passera jamais
uniquement par elles. La coexistence, elle, n'est plus un jalon : elle est structurelle et
déjà livrée, ce qu'il en restait c'était le soak déjà dû par M6.
-->

---
layout: center
class: text-center
---

<span class="eyebrow">à vous</span>

# Essayez-le <span class="grad">dans un bac à sable</span>

<div class="text-left" style="max-width: 760px; margin-inline: auto;">

```bash
kubectl create namespace crystal-backup-system
kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted --overwrite

helm install crystal-backup oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.3 -n crystal-backup-system
```

<p class="tiny muted mt-2">Pas de <code>--create-namespace</code> : le chart ne possède pas ce namespace — la KEK du cluster y vit <strong>avant</strong> l'install, et rien ne doit pouvoir l'emporter. Les labels PSA ne sont pas décoratifs : <code>helm install</code> relit le namespace et <strong>refuse</strong> si le niveau <code>enforce</code> ne colle pas.</p>

</div>

<div class="mt-6 grid grid-cols-3 gap-3 text-left" style="max-width: 760px; margin-inline: auto;">
  <div class="panel tiny"><strong>Docs & quickstart</strong><br/>crystalbackup.github.io/CrystalBackup</div>
  <div class="panel tiny"><strong>Les rapports, défauts inclus</strong><br/>…/CrystalBackup/quality</div>
  <div class="panel tiny"><strong>Le code & les ADRs</strong><br/>github.com/CrystalBackup/CrystalBackup</div>
</div>

<p class="mt-8">⭐ Si la direction vous parle : <strong>star / watch</strong> — et venez casser quelque chose,<br/>les issues avec un rapport de crucible sont les bienvenues.</p>

<!--
[44:00 → 44:30]
CTA sobre : le namespace, un helm install, trois liens, une étoile. Insister : SANDBOX.
Ne pas dérouler les labels PSA à voix haute — juste dire pourquoi le chart ne crée pas le
namespace (la KEK y est déjà, et un prune GitOps l'emporterait). Lire la page
« when not to use it » avant de s'emballer — elle est écrite pour être lue.
-->

---
layout: center
class: text-center
---

<img src="/crystal.svg" class="h-24 mx-auto mb-6" style="filter: drop-shadow(0 24px 60px rgba(43,99,240,0.55));" alt="Crystal Backup" />

# Merci ! <span class="grad">Questions ?</span>

<p class="mt-4 muted">Alexis Ducastel · alexis@infrabuilder.com</p>

<div class="mt-6 flex gap-2 justify-center">
  <span class="pill">github.com/CrystalBackup/CrystalBackup</span>
  <span class="pill grad-fill">« Le jour où on vous déçoit, vous partez avec vos données. »</span>
</div>

<!--
[44:30 → 45:00 + questions]
Si une seule chose doit rester : vos backups doivent survivre à l'outil qui les a faits.
Questions à chaud ici, le reste au buffet.
-->
