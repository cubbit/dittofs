# Checks an epic document's estimate and the order of its subtasks. Only a TOTAL line means
# the document is sound; every other line is a defect to fix. Usage:
#   awk -f epic-check.awk <epic-doc>
#
# Estimates. The header holds one `**Epic <k> estimate: <n> subtasks, <d> man-days.**`, and
# every `### T<e>.<n> <title>` heading in §6 holds one `**Subtask T<e>.<n> estimate: <d>
# man-days.**`, with <d> a multiple of 0.25. Reported: a malformed estimate line or subtask
# heading; a heading with no estimate line or two; an estimate line with no heading or under
# another subtask's; a duplicate heading; a second epic line; a subtask id of another epic; a
# header whose epic differs from the first `# Epic <k>:` title or whose figures differ from
# the sum. An epic id is a number with an optional letter (7, 7a), and the parts of a split
# epic keep the epic's subtask ids, so the subtasks of epic 7a are T7.<n>.
#
# Order. §6 lists the subtasks in order of work, so a subtask's `**Depends on:**` field names
# only subtasks above it; one naming itself or a later subtask is reported. The field runs to
# the next `**Field:**`, bullet, blank line or heading, so it may wrap; it is `none`, or ids
# written out one by one (T7.2, T7.3): a range (T7.2–T7.4, T7.2 to T7.4) is reported, since
# the ids are what tools extract. A dependency on T4.3 when §6 holds only T4.3a and T4.3b
# stands for both parts. An id with no heading here (another epic's, or the other part's of
# a split epic) is not checked.
function dep_scan(text,   tok, ends, i) {
  if (sub(/\*\*[A-Z][A-Za-z ]*:\*\*.*/, "", text)) indep = 0
  if (text ~ /^[ .]*[Nn]one([^A-Za-z]|$)/) { indep = 0; return }
  while (match(text, /T[0-9]+\.[0-9]+[a-z]?( *(–|-|to) *T?[0-9]+\.[0-9]+[a-z]?)?/)) {
    tok = substr(text, RSTART, RLENGTH); text = substr(text, RSTART + RLENGTH)
    if (tok ~ /(–|-| to )/) {
      print "subtask " cur " depends on the range " tok ": write the ids of a range out"
      gsub(/ *(–|-|to) *T?/, " T", tok)
    }
    split(tok, ends, " ")
    for (i = 1; i in ends; i++) deps[cur] = deps[cur] " " ends[i]
  }
}
function chk(who, at, dep) {
  if (pos[dep] >= at) print "subtask " who " depends on " dep ", which §6 lists " (dep == who ? "as itself" : "after it")
}
!title && /^# Epic [0-9]+[a-z]?[: ]/     { title = $3; sub(/:$/, "", title) }
/^## /                                   { in6 = ($0 ~ /^## 6\./); cur = ""; indep = 0 }
in6 && /^### /                           { cur = ""; indep = 0 }
in6 && /^### T/ && !/^### T[0-9]+\.[0-9]+[a-z]? / { print "malformed subtask heading, line " NR ": " $0 }
in6 && /^### T[0-9]+\.[0-9]+[a-z]? /     { cur = $2; head[$2]++; order[++k] = $2; pos[$2] = k
                                           if (head[$2] > 1) print "duplicate heading " $2 }
indep && (/^ *- / || /^$/ || /^#/)       { indep = 0 }
indep                                    { dep_scan($0) }
in6 && cur != "" && /\*\*Depends on:\*\*/ {
  s = $0; sub(/^.*\*\*Depends on:\*\*/, "", s); indep = 1; dep_scan(s)
}
/estimate:/ && !/(Epic [0-9]+[a-z]?|Subtask T?[0-9]+\.[0-9]+[a-z]?) estimate:/ { print "malformed, line " NR ": " $0 }
/(Epic [0-9]+[a-z]?|Subtask T?[0-9]+\.[0-9]+[a-z]?) estimate:/ {
  if ($0 ~ /^\*\*Subtask T[0-9]+\.[0-9]+[a-z]? estimate: [0-9]+(\.(25|5|75))? man-days\.\*\*$/) {
    est[$2]++; n++; d += $4
    if ($2 != cur) print "estimate line for " $2 " (line " NR ") sits under " (cur == "" ? "no subtask heading" : "heading " cur)
  } else if ($0 ~ /^\*\*Epic [0-9]+[a-z]? estimate: [0-9]+ subtasks, [0-9]+(\.(25|5|75))? man-days\.\*\*$/) {
    if (seen++) print "a second epic estimate line, line " NR
    else { epic = $2; hn = $4; hd = $6 }
  } else print "malformed, line " NR ": " $0
}
END {
  for (i = 1; i <= k; i++) if (est[order[i]] != 1) print "subtask " order[i] ": " est[order[i]] + 0 " estimate lines"
  for (id in est) if (!(id in head)) print "estimate line for " id " has no ### heading in §6"
  if (seen && title != "" && title != epic) print "the title names epic " title ", the estimate line epic " epic
  num = epic; sub(/[a-z]$/, "", num)
  if (seen) for (id in head) if (id !~ "^T" num "\\.") print "subtask " id " is not an id of epic " epic " (its subtasks are T" num ".<n>)"
  for (i = 1; i <= k; i++) {
    m = split(deps[order[i]], dl, " ")
    for (j = 1; j <= m; j++) {
      if (dl[j] in pos) { chk(order[i], i, dl[j]); continue }
      for (c = 97; c <= 122; c++) { part = dl[j] sprintf("%c", c); if (part in pos) chk(order[i], i, part) }
    }
  }
  printf "TOTAL %d subtasks, %g man-days\n", n, d
  if (!seen) print "no epic estimate line in the header"
  else if (hn != n || hd != d) printf "header says %d subtasks, %g man-days\n", hn, hd
}
