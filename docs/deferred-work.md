# Deferred work

The tracked list of WaxSeal work that was cut from an otherwise shipped
change, or that waits on a sibling repo. Work that never started does not
belong here, and the reasoning behind something deliberately not built
belongs in the doc comment beside the code it constrains; this list is
for the residuals that would otherwise survive only as a sentence in a
plan or a progress note. Agents: when you cut something, add it here in
the same change; when it lands, remove the entry. Asks of the sibling
repos live in [upstream-requests.md](upstream-requests.md), and an
`[upstream]` entry here names the ask it waits on.

Gate tags:

- `[in-repo]` nothing blocks it; it was cut for scope and is ours to
  build when picked up.
- `[upstream]` needs sibling-repo work first; the ask is in
  upstream-requests.md.

Nothing is deferred at present.
