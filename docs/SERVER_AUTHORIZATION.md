# Server authorization and credential invalidation

User accounts (password, active state, and global role) are shared across tenants.
Organization and project administrators manage memberships through the membership
endpoints. Their tokens cannot change global account attributes, even for members
of their own tenant. User directories and dashboard user lists apply the
intersection of a token's user, organization, and project restrictions. Missing
membership data never expands visibility.

Changing a global role or modifying an existing global administrator requires a
user-, organization-, and project-unrestricted grant holding every server scope,
without provider, workspace, mode, or quota restrictions. This is the same
capability set that a global administrator receives through browser login. A
standalone `admin:users:write` grant cannot promote a user or reset an administrator
password to acquire those capabilities.

A global administrator's own token is treated the same way when it is
unrestricted: the token returned by `/v1/users/login` (and `makewand user login`)
to an active `admin` account without `organization_id`/`project_id`, holding every
scope and no provider, workspace, mode, or quota restriction, can manage other
users exactly like the administrator's browser session. The account's role and
active state are re-read on every admin request, so demotion or deactivation
removes the capability immediately (and also revokes the account's tokens).
Tenant-scoped logins of an administrator, restricted administrator tokens, and
tokens of non-admin accounts keep their user binding and every boundary above.

Organizations, projects, and memberships are managed only by a global
administrator or by a tenant administrator token (organization- or
project-scoped and not bound to a user). Tokens bound to a user, including
self-service tokens that carry `admin:users:write` for password changes, cannot
create organizations or projects and cannot create or change any membership,
their own included. This prevents a user-bound token from joining an unrelated
tenant or raising its own role.

Tenant metadata follows the same intersection. A user-bound admin read token
lists only the organizations the user is an active member of, and the projects
of those organizations plus projects the user is an active member of. A parent
organization reached only through a project is shown without its budget.
Project-scoped tokens likewise see the parent organization without its budget
and receive no organization-level billing buckets or alerts.

Login tokens and administrator-issued user tokens carry a persisted opaque
`authorization_version` that binds them to the account and applicable memberships
at issuance. The server checks current account and membership state for every
model, remote-session, admin, and metrics request. Password resets, role changes,
and membership changes invalidate the binding. Re-enabling an account or restoring
a membership does not resurrect old bound credentials. Project members need an
active project and organization; an explicitly disabled organization membership
also blocks project access. Projects may have members without a separate parent
organization membership.

Admin membership updates revoke the affected user's tokens in the target
organization or project, in both the bootstrap auth file and SQLite. Organization
changes also revoke that organization's project tokens. Other organizations and
global grants remain valid. This also revokes older tokens that lack authorization
versions. Targets and payloads are validated before revocation. Account password
resets, deactivation, and global role changes revoke all tokens owned by that user.
Failed revocation returns an error and prevents the account/membership mutation. Invalidating credentials does
not cancel model requests that had already passed authorization.

Browser cookies bind to persisted account authorization state using a keyed HMAC.
Password, global-role, and active-state changes invalidate existing cookies,
including across server restarts. Existing cookies created before this binding
was introduced require a new login. Membership changes do not affect a global
administrator's browser session, whose authority is the global account role.

The SQLite token store adds `authorization_version` automatically on open. User
and membership update timestamps retain nanosecond precision. Existing databases
and older timestamp formats remain readable. Service tokens without a `user_id`
continue to use their explicit token scopes and revocation policy.

Library integrations should wrap shared authentication with
`router.WithUserAuthorization(authorizer, userStore, teamStore)` for every HTTP
surface. `HTTPHandlerWithUsers` and the `makewand serve` command do this internally.
The regression tests in `serveradmin/authorization_regression_test.go`,
`serveradmin/g8_tenant_authz_test.go`,
`router/user_authorizer_test.go`, and `serverauth/multi_manager_test.go` exercise
scope intersections, privilege conversion, persistent invalidation, failed
revocation, and provider calls using only temporary databases and local stubs.
