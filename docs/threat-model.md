# Threat model (summary)

| Threat | Mitigation in this repo | Gap / next step |
|---|---|---|
| Ransomware encrypts/deletes live M365 data | Immutable object versions (Object Lock, GOVERNANCE default, COMPLIANCE optional) + point-in-time restore | Use COMPLIANCE mode + separate AWS account for production |
| Backup bucket compromise | AES-256-GCM envelope encryption; keys not stored beside data | Move master key to KMS/Vault (currently an env var) |
| Cross-tenant data access | Tenant-scoped keys, prefixes, AAD binding | Add Postgres row-level security |
| Stolen API key | RBAC roles (admin/operator/auditor), audit log of sensitive actions | Replace static API keys with Entra ID OIDC + short-lived tokens |
| Silent corruption | SHA-256 verified on every restore; manifest hash per snapshot | Scheduled background verification jobs |
| Graph over-permissioning | Documented least-privilege permissions | `Sites.Selected` for SharePoint |
