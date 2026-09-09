# Architecture decision records

One file per decision that would otherwise have to be reconstructed from a
commit diff — where the reasoning matters more than the change, or where the
obvious alternative was rejected for a reason worth remembering.

Not every change needs one. A bug fix explains itself; a decision about *how*
the product behaves, or a tradeoff someone will be tempted to undo, does not.

Numbered in order, never renumbered. A superseded decision keeps its file and
gains a note pointing at the one that replaced it — the wrong turn is part of
the record.

Template: context (what forced a choice), decision (what we chose), and
consequences (what it costs, including what it makes harder).
