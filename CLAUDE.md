# vuka web — guide for coding agents

The server and live transport of the vuka set: routing, pages, the live
socket (`livews`), HTTP pushes, and the browser runtime (`live.js`).
Releases on one version with vuka, ui, orm and flow.

## Design philosophy

- **No new wire field, option or annotation unless necessary** — the wire
  protocol is surface too. Prefer riding an existing message or reply
  field (the one-state carry added `states` to events and `_vk_states` to
  pushes rather than a new channel). Transport-neutrality is a
  requirement: a feature that works on the live socket works on plain
  HTTP pushes unless the RFC says otherwise.
- **Frontend-framework writing feel** governs what the runtime must
  uphold: the morph keeps focus and typed input, form fields follow the
  server only when its value changed (versioned writes), `data-vk-ignore`
  and stable ids are contracts the browser side keeps for the component
  system designed in vuka's `docs/rfc/components-seamless.md`.
- A change of observed semantics rides a release note, never a patch.

## Repo facts

- `live.js` and `live.min.js` ship together — regenerate the minified
  copy with the repo's build step whenever `live.js` changes; they must
  not drift.
- Unlike the vuka root module, this module **does** track ui in go.mod:
  when ui gains API the runtime needs, bump the require to the merged ui
  commit and tidy (`GOFLAGS=-mod=mod GOPROXY=direct GOSUMDB=off go get
  github.com/vuka-lang/ui/v2@<sha>`), and verify CI-style with a scratch
  modfile + replace before pushing — a missing go.sum entry fails CI.
- Browser-side behavior changes get a twin: the Go test and the runtime
  path exercised through the live test harness.

## House rules

- Same-named branches across the vuka-lang repos are swapped into each
  other's CI builds — coordinate cross-repo changes by branch name; on
  main, a `cancelled` run superseded by a newer push is not a failure.
- Never use `git stash` (shared stash list); work in worktrees under
  `.claude/worktrees/`.
- Generic domains only in code, tests, commits and PRs — never a
  downstream app's or customer's name.
- Never wipe the Go module cache. Tag or release only on the owner's
  explicit order.
