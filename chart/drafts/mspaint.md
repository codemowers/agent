---
agentControls:
  test:
    label: Test
    enabled: true
    description: Run relevant application tests and verification. When off, skip them and report the unverified behavior.
  build:
    label: Build
    enabled: true
    description: Build application artifacts and container images. When off, do not build publishable artifacts or push images; compilation required for enabled tests is allowed.
  deploy:
    label: Deploy
    enabled: true
    description: Deploy the application in the assigned namespace using verified artifacts. When off, prepare manifests but do not deploy or update application workloads. Respect the Build control.
  public_gallery:
    label: Public gallery
    enabled: false
    description: When off, drawings stay private unless an owner explicitly creates a share link. Do not publish a public browsing gallery.
---

# Paint: OIDC drawing app with S3 saves and sharing

Build and deploy a usable browser drawing application inspired by classic MS Paint.

## User experience

- A welcoming sign-in page, then a dashboard of the user's saved drawings.
- An editor with a large canvas, clear toolbar, color palette, stroke width,
  keyboard shortcuts, zoom, undo/redo and a visible save status.
- Tools: pencil/brush, eraser, flood fill, straight line, rectangle, ellipse,
  text and color picker. Support mouse and touch/pointer input.
- New drawing, rename, duplicate, delete, PNG import and PNG export. Warn before
  discarding unsaved edits. Document the maximum canvas size and upload limit.
- Save editable drawing state as well as a PNG preview, so reopening a drawing
  preserves the work. Include enough state for subsequent editing; PNG alone
  is not an editable project format.
- Owners can create and revoke read-only share links. Recipients do not need
  OIDC to view a shared drawing and download its PNG. Sharing never grants edit
  access. The S3 bucket stays private.

## Identity and ownership

Use OIDC sign-in.

Persist owners as the verified issuer/subject pair, not a username supplied by
the browser. Enforce ownership on every read/write/delete/share operation.
Give unauthorized callers no access to another user's private objects. Share
tokens authorize only the selected drawing's read-only view and are stored
hashed; revocation takes effect for subsequent requests.

## Storage

Store project JSON and PNG previews in the private S3 bucket. Validate upload
content and size. Use opaque drawing IDs and derive object keys on the server;
do not accept arbitrary bucket names or object paths from the browser. Use
atomic version/pointer updates so failed saves cannot destroy the last good
version. Autosave is debounced, reports failures and supports retry.

Persist drawing metadata, owner identity and revocable share records durably.
Keep metadata and S3 consistency explicit. Deleting a drawing also cleans up
its objects, with retryable orphan cleanup.

## Completion

Deliver code, usage documentation and the deployed application URL when Deploy
is enabled. When Test is enabled, verify drawing, undo/redo, save/reload,
cross-user isolation, anonymous share access and revocation. Respect the Test,
Build and Deploy controls; report skipped work and unresolved limitations.
