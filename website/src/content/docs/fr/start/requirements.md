---
title: Prérequis
description: Ce que votre cluster, votre stockage et votre stockage objet doivent fournir avant d'installer Crystal Backup.
sourceFile: src/content/docs/start/requirements.md
sourceHash: 29a56bf478978c2cdc27a3a3f7cb2d48124a3a5b
---

## Vérifiez votre cluster avant d'installer

Tout ce que contient cette page peut être vérifié automatiquement, contre le cluster que
vous avez réellement, par un script qui **n'installe rien et ne change rien**. Il ne crée
aucun objet, n'écrit aucun fichier — pas même temporaire — et n'émet jamais autre chose que
`kubectl get`, `kubectl version` et `kubectl auth can-i`.

Il existe surtout pour répondre à une question que cette page ne peut décrire que dans
l'abstrait : **lesquelles de vos StorageClasses auront réellement leurs données backupées,
et lesquelles seront skippées.** Pour chaque StorageClass, il résout l'exposer que
CrystalBackup choisirait — `cephfs-shallow`, `csi-generic`, ou *skipped* avec le reason
`CSISnapshotUnsupported` — et vous dit combien de PVCs reposent sur chacune. Découvrir au
bout de trois semaines qu'un namespace n'a jamais été protégé parce que son driver CSI ne
sait pas faire de snapshot est une mauvaise façon de l'apprendre.

Cette table de routage n'est pas écrite à la main. Elle est **générée depuis le code de
sélection de l'operator lui-même** (`internal/exposer`) et tenue à lui par un garde-fou CI,
si bien que le script ne peut pas dériver vers la description d'une version de la logique
qui n'existe plus.

### Le télécharger, le lire, l'exécuter

```bash
BASE=https://crystalbackup.github.io/CrystalBackup
curl -fsSLO "$BASE/preflight.sh"
curl -fsSLO "$BASE/preflight.sh.sha256"
curl -fsSLO "$BASE/preflight.sh.cosign.bundle"

# 1. the checksum
sha256sum -c preflight.sh.sha256          # macOS: shasum -a 256 -c preflight.sh.sha256

# 2. the signature — keyless, the same Sigstore trust root as our container images
cosign verify-blob preflight.sh \
  --bundle preflight.sh.cosign.bundle \
  --certificate-identity-regexp '^https://github\.com/CrystalBackup/CrystalBackup/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 3. read it — plain POSIX shell, and its header states exactly what it does
less preflight.sh

# 4. run it
sh preflight.sh
```

`--json` donne les mêmes constats sous forme de document lisible par une machine, pour
l'automatisation. `jq` est utilisé s'il est présent et n'est pas requis ; sans lui le script
le dit et retombe sur un encodeur intégré.

Codes de sortie : **0** prêt, **1** prêt avec réserves, **2** bloquant, **3** n'a pas pu
être évalué du tout. Une vérification qui n'a *pas pu être faite* — une permission qui vous
manque, une CRD qu'il n'a pas pu lire — est rapportée comme telle et atterrit en code 1.
Elle n'est jamais comptée comme réussie.

### Ou bien, le one-liner

```bash
curl -fsSL https://crystalbackup.github.io/CrystalBackup/preflight.sh | sh
```

C'est un raccourci, et c'est un vrai compromis : piper une URL dans un shell exécute ce que
le serveur renvoie, quel qu'il soit, et ni la somme de contrôle ni la signature ne sont
vérifiées. C'est très bien pour un cluster jetable, et c'est une envie raisonnable. Pour
quoi que ce soit qui compte, utilisez les quatre étapes ci-dessus — elles prennent une
vingtaine de secondes de plus et elles sont la raison pour laquelle nous publions la somme
de contrôle et la signature.

## Kubernetes

**Version 1.30 ou ultérieure.** C'est un plancher dur, pas une recommandation : le modèle
d'admission repose sur `ValidatingAdmissionPolicy`, qui a atteint le GA en 1.30. Le chart
Helm déclare `kubeVersion: ">= 1.30.0-0"` et refusera de s'installer en dessous.

Il vous faudra être cluster-admin pour installer : le chart crée des CRDs, du RBAC
cluster-scoped, des policies d'admission et un namespace.

## Stockage — le chemin snapshot CSI

Les backups sont pris depuis un **snapshot en lecture seule**, pas depuis le volume vivant.
Cela demande :

- l'**API `snapshot.storage.k8s.io/v1`** — les CRDs de l'external-snapshotter et le
  snapshot controller, installés dans le cluster ;
- au moins une **`VolumeSnapshotClass`** pour les drivers CSI qui portent les PVCs que vous
  voulez protéger ;
- un driver CSI qui gère les snapshots.

Une PVC sur un driver incapable de snapshot est **skippée**, pas abandonnée
silencieusement : le volume est rapporté avec `status.volumes[].phase: Skipped` et
`reason: CSISnapshotUnsupported`. Cela vaut la peine d'être vérifié avant de supposer qu'un
namespace est couvert.

Des chemins conscients du stockage existent pour CephFS (`backingSnapshot` shallow, une
lecture sans copie) et RBD (clone copy-on-write). Tout autre CSI capable de snapshot prend
le chemin générique : le `VolumeSnapshotContent` est re-lié dans `crystal-backup-system`
comme paire statique avec une PVC copy-on-write temporaire.

**Les PVCs en `volumeMode: Block` ne sont pas supportées.** Elles sont rapportées comme des
échecs par volume avec le reason `RestoreBlockUnsupported`.

## Stockage objet

Un stockage objet compatible S3, joignable depuis le cluster. Testé contre AWS S3, MinIO,
SeaweedFS et Ceph RGW.

Par location, il vous faut :

- un **endpoint**, un **bucket** et éventuellement un **prefix** ;
- des **credentials** avec lecture et écriture sur le prefix. La plupart des gateways
  non-AWS ont aussi besoin de `forcePathStyle: true` ;
- si l'endpoint utilise une CA privée, son bundle PEM pour `spec.s3.caBundle`.

:::caution[Les movers détiennent aujourd'hui les credentials root du bucket]
Des credentials de mover à durée de vie courte et limités au repository ne sont pas
implémentés dans cette release. Chaque Job de mover reçoit les credentials de la location
tels quels, si bien qu'un mover compromis peut atteindre tout ce que ces credentials peuvent
atteindre. Limitez la portée des credentials au bucket — ou au prefix — côté stockage objet,
et donnez à chaque location les siens.
:::

## Réseau

Le chart installe des NetworkPolicies default-deny pour `crystal-backup-system` avec des
autorisations étroites. Deux conséquences en découlent :

- **L'application, c'est le travail de votre CNI.** Certains CNI acceptent les objets
  NetworkPolicy et n'appliquent rien (le `kindnet` par défaut de Kind en fait partie). Leur
  présence n'est pas la preuve que le confinement tient — vérifiez-le sur votre CNI.
- **Un endpoint S3 sur une adresse privée demande une règle explicite.** Les movers se
  voient refuser par défaut l'egress vers `10.0.0.0/8`, `172.16.0.0/12`,
  `192.168.0.0/16`, `169.254.0.0/16` et `127.0.0.0/8` sur le port 443, pour empêcher un
  mover compromis de pivoter vers les services internes au cluster. Un endpoint S3
  on-premises situé dans ces plages a besoin d'une entrée dans
  `networkPolicy.extraMoverEgress`.

## La cluster KEK — provisionnez-la avant d'installer

Pour le plan cluster, la clé du repository de la plateforme est un mot de passe restic
aléatoire wrappé par une **identité age X25519** : la *cluster KEK*.

**Ni le chart ni l'operator ne la génèrent jamais.** Une clé née dans le cluster serait
perdue avec le cluster, et chaque backup avec elle. Vous la générez hors bande, vous la
mettez sous séquestre **à l'extérieur** du cluster, et vous la provisionnez vous-même comme
Secret.

```bash
age-keygen -o cluster-kek.txt
```

Mettez `cluster-kek.txt` sous séquestre là où vous gardez vos secrets racines — un
gestionnaire de mots de passe, un HSM, une enveloppe scellée. **C'est l'entrée du disaster
recovery.** Sans elle, un bucket plein de backups est un bucket plein de chiffré.

Le plan namespace n'a besoin de rien d'équivalent : le mot de passe du repository de chaque
tenant lui appartient, soit fourni par lui, soit généré dans son propre namespace.

## Pod Security Admission

Le namespace de l'operator est labellisé `enforce: baseline` (avec `audit` et `warn` à
`restricted`). L'operator lui-même est conforme à `restricted`, mais les data movers
tournent en `runAsUser: 0` avec `DAC_OVERRIDE` — ils doivent préserver la propriété des
fichiers au restore — ce que `restricted` refuserait. Cet assouplissement ne s'applique qu'à
`crystal-backup-system` ; rien ne change dans les namespaces des tenants.

## Dimensionnement

L'operator est petit : `10m` de CPU et `64Mi` de mémoire demandés, `500m`/`256Mi` en limits.

Le travail se passe dans les Jobs de mover, un par PVC, et le nombre qui tourne
simultanément est plafonné à l'échelle du cluster par `maxConcurrentMovers`. Dimensionnez la
capacité de vos nœuds pour cette concurrence, pas pour l'operator.

La seule chose qui passe à l'échelle du volume total de données plutôt que par namespace est
`restic prune` sur le repository partagé — il est gourmand en mémoire et prend une fenêtre
exclusive. Donnez-lui des heures creuses et bornez-le avec `pruneMaxRepackSize`. Voir
[Maintenance et vérification](/CrystalBackup/fr/docs/guides/maintenance/).

## Optionnel

- **Prometheus Operator** — pour `metrics.serviceMonitor.enabled: true`. Sans lui, les
  métriques sont quand même servies sur le port 8443 en HTTPS avec authn/authz de
  l'API server ; scrapez-les comme bon vous semble.
- **Un outil de backup existant.** La coexistence est un objectif de conception, pas une
  arrière-pensée. Ajoutez son namespace à `admission.deniedNamespaces` pour que les
  ressources de Crystal Backup destinées aux tenants ne puissent pas y être créées.

Ensuite : [Installer avec Helm](/CrystalBackup/fr/docs/start/install/).
