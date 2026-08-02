# The end-of-soak restore drill

A soak that ends without a restore has proved that the program does not crash. It has proved
nothing about your data.

This is the part of the fortnight that answers the question the product exists for, and it is the
single most valuable thing you can send back. It takes about an hour, most of which is waiting.

**The shape:** restore one namespace into a **scratch namespace that does not exist yet**, then
compare the restored volume against the source, property by property. Restoring over the source
would make the exercise dishonest — an attribute the restore failed to re-apply would still be
sitting there from before, and the comparison would come back clean for the wrong reason. A fresh
namespace means every byte and every attribute you find was put there by the restore.

---

## 0. What you need

- one namespace from the soak scope, ideally the most awkward one — many small files, odd
  permissions, a volume that is not the smallest;
- a namespace name that does not exist. Check: `kubectl get ns <name>` must say NotFound;
- an image with GNU coreutils, `attr` and `acl`. `debian:stable-slim` plus
  `apt-get install -y attr acl` is what this runbook assumes;
- `fidelity-manifest.sh` from this directory.

The comparison pod needs to run as **root** to read every file and see every attribute. If your
cluster enforces Pod Security Admission `restricted` on new namespaces by default, the drill
namespaces need `pod-security.kubernetes.io/enforce=privileged` — label them before you start, or
the drill pod will not admit and you will spend twenty minutes on the wrong problem.

---

## 1. Quiesce, then take one last backup

The source volume has been changing throughout the soak. If you back it up at 14:00 and capture
its manifest at 14:20, a diff will show every file the workload touched in between — twenty
minutes of ordinary churn, indistinguishable at a glance from a restore defect.

So, for the one namespace you are drilling:

```sh
# scale the workload down, so the volume stops changing
kubectl -n <source-ns> scale deploy --all --replicas=0
kubectl -n <source-ns> get pods -w        # wait until nothing is running

# one last on-demand backup of the now-quiet volume
kubectl -n <source-ns> annotate backupschedule <name> \
  crystalbackup.io/trigger="$(date -u +%s)" --overwrite
# ...or create a Backup by hand, whichever your install uses
kubectl -n <source-ns> get backups -w     # wait for Completed
```

**If you cannot quiesce it** — and on a real cluster you often cannot — do the drill anyway and
say so in your report. Ordinary churn shows up as differences in *content, size and mtime on
files the workload was writing*. It does **not** explain a lost setuid bit, a vanished ACL, a
broken hardlink, an mtime that lost its nanoseconds, or a file that is simply absent. Those stay
readable through the noise, and they are what this drill is for.

Note the backup's name and the time it completed. You will need both.

---

## 2. Capture the source manifest

```sh
kubectl -n <source-ns> apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: fidelity-before
spec:
  restartPolicy: Never
  containers:
    - name: tools
      image: debian:stable-slim
      command: ["sleep", "3600"]
      securityContext:
        runAsUser: 0
      volumeMounts:
        - name: data
          mountPath: /data
          readOnly: true
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: <source-pvc>
        readOnly: true
YAML

kubectl -n <source-ns> wait --for=condition=Ready pod/fidelity-before --timeout=5m
kubectl -n <source-ns> exec fidelity-before -- \
  sh -c 'apt-get -qq update && apt-get -qq install -y attr acl' >/dev/null

kubectl -n <source-ns> exec -i fidelity-before -- bash -s -- /data \
  < fidelity-manifest.sh > before.txt
```

Read the header of `before.txt` before going further:

```
# extended attributes compared
# POSIX ACLs          compared
# mtime resolution    nanosecond
```

If any of those says **NOT COMPARED**, stop and fix the image. A facet that is not measured
compares equal on both sides, and the drill would report success on exactly the properties most
likely to have regressed. The script refuses to run at all unless you override it, for that
reason.

Note the entry count. If it is not roughly what you expect for that volume, find out why now.

---

## 3. Restore into a namespace that does not exist

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: soak-drill
spec:
  source:
    locationRef: { name: <your-location> }
    namespace: <source-ns>            # as it was named at backup time
    backup: <the backup from step 1>  # or: time: latest
  target:
    namespace: <source-ns>-drill      # MUST NOT EXIST
    createNamespace: true
  mode: Recreate
  confirmation: <source-ns>-drill     # the TARGET namespace
```

```sh
kubectl apply -f restore.yaml
kubectl get clusterrestore soak-drill -w
```

**Write down how long it took**, and against what — the volume's size, its file count, and the
object store's location relative to the cluster. A restore duration measured on real data over a
real link is one of the numbers this whole soak exists to produce, and there is no metric in the
archive that substitutes for you having watched it.

While it runs, note anything the operator says: `kubectl describe clusterrestore soak-drill`, and
the events in the target namespace. If it fails, that is the drill's result — capture it in full
and send that. A failed restore found on day fourteen is worth more than a successful one.

---

## 4. Capture the restored manifest

The restored PVC is in a different namespace, and a pod can only mount PVCs from its own
namespace — so this is a second pod, not the same one.

```sh
kubectl -n <source-ns>-drill get pvc        # confirm the PVC came back, and its size and class
```

Apply the same pod as in step 2, in `<source-ns>-drill`, with `name: fidelity-after` and the
restored PVC's name. Then:

```sh
kubectl -n <source-ns>-drill exec -i fidelity-after -- bash -s -- /data \
  < fidelity-manifest.sh > after.txt
```

---

## 5. Compare

```sh
diff -u before.txt after.txt
```

**An empty diff is the strongest possible result.** Say so plainly in your report; it is not a
boring outcome, it is the one the beta bar is set against.

The `# root` header line will differ (the two paths are the same, but check). Everything else
should match. When it does not, the columns are:

```
path(b64)  type  mode  uid  gid  nlink  size  mtime  density  linkgroup  content  xattr|acl
```

Paths are base64 so that a name containing a tab, a newline or a quote cannot corrupt the record.
To read one: `printf '%s' 'aGFyZGxpbmsudHh0' | base64 -d`.

| what differs | what it means |
|---|---|
| **content** (the sha256) | the bytes came back different. The most serious finding there is. Report it with the path. |
| **a whole record is missing** | a file was not restored at all. Equally serious. |
| **mode** | permissions lost. Look especially at the leading digit: `4755` → `755` is a **lost setuid bit**, and that silently breaks anything that relied on it. |
| **uid / gid** | numeric ownership not preserved. Check whether your storage class and CSI driver do UID mapping — some do, and then this is about the driver rather than about the restore. Say which you are using. |
| **mtime** | `…123456789` → `…000000000` is **nanosecond precision lost**, which is what breaks incremental tools and rsync-style comparisons downstream. A wholly different timestamp on a file the workload was writing during step 1 is churn, not a defect. |
| **nlink + linkgroup** | `2 / link:…` → `1 / -` means a **hardlink pair came back as two independent copies**. The data is intact and the volume quietly uses twice the space, forever, and every future edit diverges. Report it. |
| **xattr** (before the `\|`) | an extended attribute did not survive. Decode both sides to see which: `printf '%s' '<b64>' \| base64 -d`. |
| **acl** (after the `\|`) | a POSIX ACL did not survive. Before reporting it, check that the restored volume's filesystem even supports ACLs — some CSI drivers mount without them, and then this is a fact about the target, not the restore. Note which you have. |
| **density** `sparse` → `dense` | a sparse file was restored fully allocated. No data is lost; the volume may be very much larger than the original. Worth reporting with the size. |
| **type** | a FIFO, a symlink or a directory came back as something else. Rare and serious. |

A quick triage that separates the two kinds of finding:

```sh
# records that differ in metadata but NOT in content — these are never churn
diff before.txt after.txt | grep -E '^[<>]' | awk -F'\t' '{print $11}' | sort | uniq -c | sort -rn | head
```

Any hash appearing exactly twice in that list is one file whose bytes are identical and whose
metadata is not — which is a defect, not a workload writing to its volume.

---

## 6. Check the Kubernetes objects too, briefly

The volume is the point, but the restore also recreated objects:

```sh
kubectl -n <source-ns>-drill get pvc <pvc> -o jsonpath='{.spec.resources.requests.storage}{"\t"}{.spec.storageClassName}{"\t"}{.spec.accessModes}{"\n"}'
kubectl -n <source-ns> get pvc <pvc> -o jsonpath='{.spec.resources.requests.storage}{"\t"}{.spec.storageClassName}{"\t"}{.spec.accessModes}{"\n"}'

kubectl -n <source-ns>-drill get all
```

Capacity, storage class and access modes should match the source (they come from the snapshot's
own tags). A workload that came back with a different replica count, a missing ConfigMap or a
Secret that is present but empty is worth a line in the report.

---

## 7. Clean up, and what to send

```sh
kubectl -n <source-ns> delete pod fidelity-before
kubectl delete ns <source-ns>-drill          # takes the restored PVC with it
kubectl -n <source-ns> scale deploy --all --replicas=<what it was>
```

Send, alongside the soak archive:

- `before.txt` and `after.txt`, or just the `diff -u` if they are large. **They contain your file
  names**, base64-encoded but not redacted — this is the one part of the soak the kit does not
  anonymise for you, because a fidelity comparison without paths cannot be acted on. If that is
  not acceptable, send the diff with the path column stripped
  (`cut -f2-`) and keep the mapping yourself; the finding is still readable and we will ask you
  to decode specific rows.
- the restore duration, the volume size and file count, the storage class and CSI driver, and the
  object store's provider and region;
- whether you were able to quiesce the workload first;
- anything the operator said while it ran.

And if the restore failed outright: send that instead, in full. It is the more useful result.
