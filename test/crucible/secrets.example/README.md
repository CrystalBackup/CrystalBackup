# Crucible credentials layout

Copy these files (without the `.example` suffix) into `.secrets/` at the
**repository root** — that directory is git-ignored and is the only place the
crucible reads credentials from:

```
<repo>/.secrets/
├── HETZNER_TOKEN        # raw Hetzner Cloud API token (Project > Security > API tokens, read+write)
├── HETZNER_S3           # Object Storage keypair    (Project > Security > S3 credentials)
├── crystalbackup.key    # SSH private key (chmod 600)
└── crystalbackup.key.pub
```

The **public key must be registered in your Hetzner Cloud project** under the
name `crystalbackup` (Project > Security > SSH keys) — or pass another name via
`TF_VAR_ssh_key_name`.

To point somewhere else entirely: `export CRYSTALBACKUP_SECRETS_DIR=/path/to/secrets`.

Nothing under `.secrets/` is ever committed, copied into artifacts, or echoed
by the tooling.
