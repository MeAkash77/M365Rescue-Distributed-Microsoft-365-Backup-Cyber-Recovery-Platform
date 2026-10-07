# Connecting a real Microsoft 365 tenant

Demo tenants need none of this. For real data:

1. **Entra ID → App registrations → New registration** (multi-tenant if you will protect customer tenants).
2. **Certificates & secrets** → create a client secret → set `AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` in `.env`.
3. **API permissions → Microsoft Graph → Application permissions** (least privilege):
   - Backup: `User.Read.All`, `Files.Read.All`, `Mail.Read`
   - Restore: `Files.ReadWrite.All`, `Mail.ReadWrite`
   - Prefer `Sites.Selected` / resource-specific consent when you later add SharePoint.
4. **Grant admin consent** in the target tenant.
5. Create the tenant: `POST /api/v1/tenants {"name":"Acme","ms_tenant_id":"<directory-id>"}` then trigger a backup.

Implemented for real tenants: OneDrive (drive delta) and Exchange Online inbox (message delta). SharePoint and Teams return
"not supported" by design (roadmap). Restore to real tenants is best-effort and has not been validated against a live tenant here.
