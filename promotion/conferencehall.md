# ConferenceHall — dossier de soumission

> Prêt à copier-coller dans conference-hall.io (ou tout CFP équivalent : Sessionize,
> Papercall…). Les champs suivent la structure ConferenceHall : titre, résumé (public),
> description (relecteurs), niveau, format, langue, notes aux organisateurs, bio.
> Les variantes en fin de fichier servent pour les KCD et formats plus courts.

---

## Titre

**Chaque namespace mérite ses backups — self-service, multi-tenant et sans lock-in sur Kubernetes**

Variantes (selon la place disponible et le ton de l'événement) :

- *Le cluster est sauvegardé. Ses tenants, non.* (plus provocateur, marche très bien en meetup)
- *Chaque namespace mérite ses backups* (court, pour les grilles serrées)
- *Backup Kubernetes multi-tenant : et si on arrêtait de mentir ?* (à réserver aux salles qui aiment le second degré)

## Résumé (abstract public — ~150 mots)

Votre plateforme Kubernetes multi-tenant est sauvegardée : Velero tourne toutes les nuits,
c'est l'admin qui a la main. Et vos tenants ? Aucun moyen de déclencher un backup, aucune
visibilité sur ce qui est protégé, aucune copie hors plateforme sous leur propre clé.

Crystal Backup est un opérateur open source (Apache-2.0) qui comble exactement ce trou :
DR cluster pour l'admin **et** backups self-service pour les namespaces — dépôts restic
standards (zéro lock-in, relisibles sans l'opérateur), isolation tenant structurelle,
droit à l'effacement RGPD, et restauration possible depuis un bucket nu, sans aucun CR
survivant.

En démo live : on sauvegarde, on détruit le namespace, on le fait revenir — puis on relit
le dépôt avec le restic upstream, et on regarde l'API refuser un CR forgé.

Et parce que « no bullshit » : chaque milestone est accepté sur un vrai cluster
RKE2 + Rook-Ceph, et les rapports — défauts inclus — sont publiés. On vous racontera les
meilleurs bugs.

## Description (pour les relecteurs / comité)

**Le problème.** Les plateformes Kubernetes managées isolent leurs tenants par namespace,
et les sauvegardent avec un outil cluster-wide admin-only. Résultat côté tenant : trois
frustrations — aucune action possible, aucune visibilité, aucune porte de sortie. Aucun
outil existant (Velero, K8up, VolSync, Kasten) ne couvre la combinaison
*self-service multi-tenant + réversibilité + DR directement depuis le dépôt* ; chacun en
résout une partie, les trous diffèrent.

**La proposition.** Crystal Backup, opérateur Kubernetes Apache-2.0, deux plans façon
cert-manager :

- un **plan cluster** : l'admin sauvegarde tous les namespaces (données PVC **et**
  manifests assainis) dans un dépôt restic partagé — le DR de la plateforme ;
- un **plan namespace** : chaque tenant peut *en plus* sauvegarder chez lui — son bucket,
  ses credentials, **sa clé, que la plateforme ne possède pas** (et n'a structurellement
  aucun moyen de posséder).

Points saillants traités dans le talk : dépôts restic **standards** relisibles avec l'outil
upstream (la réversibilité comme garantie de format, pas comme promesse marketing) ;
le dépôt comme **source de vérité** (`kubectl get backups` liste exactement ce qui est
restaurable, DR possible depuis un bucket nu) ; isolation par **construction** (le champ
qui permettrait de tricher n'existe pas) ; snapshots CSI « least data movement »
(CephFS shallow, clone COW RBD) ; droit à l'effacement RGPD physique ; coexistence
revendiquée avec l'outil de backup déjà en place.

**Le fil rouge honnêteté.** Le projet est développé avec une assistance IA importante,
sous direction et revue humaines — et c'est précisément pour ça que la vérification est
brutale : chaque milestone est accepté sur une plateforme réelle jetable
(RKE2 + Rook-Ceph + Longhorn sur Hetzner), avec un **oracle restic indépendant** (le
contrôleur ne note pas sa propre copie), et les 82 checks sont publiés — passes, skips
**et défauts trouvés**. Le talk assume les war stories : la signature cosign accrochée au
mauvais manifest pendant quatre releases, le backup qui répond `Completed` sans avoir rien
écrit, le droit à l'oubli qui n'oubliait rien. Ce qui n'est **pas** livré (CLI, UI,
immutabilité S3 Object Lock) est annoncé tel quel.

**Démo live (~12 min, 100 % locale, sans réseau)** : kind + S3 in-cluster.
Backup d'un namespace → `kubectl delete namespace` → restauration depuis la coordonnée du
dépôt → vérification du checksum → lecture du dépôt avec le restic upstream (aucun
composant Crystal Backup impliqué) → tentative de CR forgé refusée en direct par
l'admission. Plan B enregistré en cas de démon de la démo.

**Le public repart avec** : une grille de lecture pour évaluer n'importe quel outil de
backup K8s (réversibilité, isolation, source de vérité, preuve de restauration), des
patterns d'architecture réutilisables (projection vs source de vérité, isolation par
absence de mécanisme, cascade type CronJob), et l'envie de tester — en sandbox — un
projet jeune qui documente ses propres trous.

**Niveau** : intermédiaire. Prérequis : avoir déjà touché un cluster (PVC, CRD, opérateur).
Aucune connaissance de restic requise.

## Format

- **Talk 45 min** : ~30 min de présentation + ~12 min de démo live + questions au buffet.
- Langue : **français** (slides bilingues-friendly : titres FR, termes techniques EN).
- Adaptable en 35 min (voir variantes) et en anglais sur demande.

## Notes aux organisateurs

- Démo 100 % locale (kind sur le laptop) : **aucune dépendance au réseau de la salle**.
  Un écran HDMI/USB-C standard suffit.
- Plan B intégré : la démo est aussi enregistrée (replay terminal) si la machine décide
  de vivre sa vie.
- Le projet est open source (Apache-2.0), le talk ne vend rien ; les rapports
  d'acceptation cités sont publics et vérifiables :
  <https://crystalbackup.github.io/CrystalBackup/quality/>
- Le speaker peut fournir le deck en PDF avant l'événement.

## Bio speaker (à ajuster)

> Alexis Ducastel — fondateur d'infraBuilder, travaille sur les plateformes Kubernetes
> managées et leurs angles morts. Développe Crystal Backup en open source, avec une
> assistance IA assumée et des tests sur vrai cluster pour compenser. Vient raconter les
> bugs autant que les features.
>
> *(Compléter avec certifs/talks précédents selon l'événement : les comités KCD aiment
> les références de scène.)*

## Le pitch en 20 mots (pour les formats micro)

> Vos tenants n'ont ni backup self-service, ni visibilité, ni porte de sortie.
> On répare ça — et on le prouve.

---

## Variantes par événement

### Meetup local (45 min) — la version de ce dossier
Tout le contenu, démo complète, war stories détendues.

### KCD / conférence (35 min)
Il faut trouver ~10 min : couper la slide « 8 étapes d'un backup » (~1 min 30, garder la
cascade), une des trois war stories (~1 min, garder cosign + `Completed`-sans-écrire),
le meme Drake (~30 s), resserrer la démo à 9 min en préparant la `ClusterBackupLocation`
à l'avance (le `restic init` est déjà fait, on commence au `ClusterBackup`), et compresser
l'acte 1 de 8 à 5 min (fusionner « Il est 9h04 » avec les trois frustrations, passer plus
vite sur le tableau comparatif). Les minutages des notes restent la référence : recaler
après une répétition chronométrée.

### Lightning (10 min)
Un seul angle : « le backup qui dit Completed sans rien écrire » — la war story, l'oracle
restic indépendant, le gate de fidélité, et le CTA. Pas de démo live, une capture de
`restic snapshots` suffit.

### Tags / catégories suggérés
`kubernetes` · `backup` · `disaster-recovery` · `multi-tenancy` · `open-source` ·
`storage` · `sre` · `platform-engineering` · `gdpr`
