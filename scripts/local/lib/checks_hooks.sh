# shellcheck shell=sh
# Checks W1 to W3 and G1 to G4. Source this file.
# The webhook checks send deliveries that are signed like GitHub signs them
# (X-Hub-Signature-256, HMAC-SHA256 of the body with the webhook secret of the
# stack). The server checks the signature with the secret of the GitHub
# record of the owner (config.webhook_secret_ref): fx_hook_secret writes the
# webhook secret of the stack to the team secret HOOK_SECRET_REF. The
# templates are in scripts/local/webhooks/.

WEBHOOK_OWNER=of-local-checks
WEBHOOK_REPO=webhook-demo
BOT_JSON='{"login":"of-local-checker[bot]","id":1,"type":"Bot"}'
HOOK_SECRET_REF=LOCAL_WEBHOOK_SECRET

# hook_secret  Print the webhook secret of the stack.
hook_secret() {
	need_file "$LOCAL_DIR/webhook.secret"
	tr -d '\n' <"$LOCAL_DIR/webhook.secret"
}

# hook_sign SECRET FILE  Print sha256=<hex> for a file.
hook_sign() {
	_hs=$(openssl dgst -sha256 -hmac "$1" -hex "$2" 2>/dev/null | sed 's/^.*= *//')
	printf 'sha256=%s' "$_hs"
}

# hook_deliver EVENT DELIVERY_ID BODY_FILE [SIGNATURE]  POST a delivery. It sets
# HOOK_CODE and HOOK_BODY. Without SIGNATURE the delivery is signed with the
# secret of the stack. SIGNATURE=none sends no signature header.
hook_deliver() {
	need_file "$LOCAL_DIR/webhook.secret"
	_hd_event=$1
	_hd_id=$2
	_hd_file=$3
	_hd_sig=${4:-}
	[ -n "$_hd_sig" ] || _hd_sig=$(hook_sign "$(hook_secret)" "$_hd_file")
	if [ "$_hd_sig" = none ]; then
		set --
	else
		set -- -H "X-Hub-Signature-256: $_hd_sig"
	fi
	HOOK_CODE=$(curl -sS -m 30 -X POST -H "X-GitHub-Event: $_hd_event" -H "X-GitHub-Delivery: $_hd_id" \
		-H 'Content-Type: application/json' "$@" --data-binary @"$_hd_file" -o "$WORK/hook.out" -w '%{http_code}' \
		"${OF_SERVER%/}/integrations/github/webhook" 2>>"$CHECK_LOG") || HOOK_CODE=000
	HOOK_BODY=$(cat "$WORK/hook.out" 2>/dev/null || true)
	log "delivery $_hd_id ($_hd_event): HTTP $HOOK_CODE $(printf '%s' "$HOOK_BODY" | cut -c1-160)"
}

# fx_hook_secret  Write the webhook secret of the stack to the team secret
# HOOK_SECRET_REF.
fx_hook_secret() {
	api_call PUT "/secrets/$HOOK_SECRET_REF" "$(jq -nc --arg v "$(hook_secret)" '{value: $v}')"
	case $API_CODE in
	2??) ;;
	404) api_ok POST /secrets "$(jq -nc --arg n "$HOOK_SECRET_REF" --arg v "$(hook_secret)" '{name: $n, value: $v}')" ;;
	*) fail "PUT /secrets/$HOOK_SECRET_REF answered $API_CODE: $(printf '%s' "$API_BODY" | cut -c1-200)" ;;
	esac
}

# fx_record_hook_secret RECORD_JSON  Set config.webhook_secret_ref of a GitHub
# record to HOOK_SECRET_REF when it has another value.
fx_record_hook_secret() {
	[ "$(printf '%s' "$1" | jq -r '.config.webhook_secret_ref // empty')" != "$HOOK_SECRET_REF" ] || return 0
	api_ok PATCH "/integrations/$(printf '%s' "$1" | jq -r .id)" \
		"$(printf '%s' "$1" | jq -c --arg w "$HOOK_SECRET_REF" '{config: ((.config // {}) + {webhook_secret_ref: $w})}')"
	log "set the webhook secret of the GitHub integration record $(printf '%s' "$1" | jq -r .id) to $HOOK_SECRET_REF"
}

# ---------- W1 ----------

check_W1() {
	need_cmd curl jq openssl
	fx_webhook_integration
	jq --arg r "$WEBHOOK_OWNER/$WEBHOOK_REPO" '.repository.full_name = $r' "$HERE/webhooks/ping.json" >"$WORK/ping.json"
	hook_deliver ping "of-local-w1-$(rand_token)" "$WORK/ping.json"
	[ "$HOOK_CODE" = 200 ] || fail "a signed ping got HTTP $HOOK_CODE, not 200: $(printf '%s' "$HOOK_BODY" | cut -c1-200)"
	[ "$(printf '%s' "$HOOK_BODY" | jq -r '.status // empty')" = pong ] || fail "the answer to the ping is not {\"status\":\"pong\"}: $HOOK_BODY"
	evidence "a signed ping gets 200 {\"status\":\"pong\"} on /integrations/github/webhook"
	pass
}

# ---------- W2 ----------

check_W2() {
	need_cmd curl jq openssl
	fx_webhook_integration
	jq --arg r "$WEBHOOK_OWNER/$WEBHOOK_REPO" '.repository.full_name = $r' "$HERE/webhooks/ping.json" >"$WORK/ping.json"
	# A signature of another secret.
	_w2_bad=$(hook_sign "not-the-secret-$(rand_token)" "$WORK/ping.json")
	hook_deliver ping "of-local-w2a-$(rand_token)" "$WORK/ping.json" "$_w2_bad"
	[ "$HOOK_CODE" = 401 ] || fail "a delivery with a wrong signature got HTTP $HOOK_CODE, not 401"
	[ "$(printf '%s' "$HOOK_BODY" | jq -r '.error.code // empty')" = invalid_signature ] || fail "the 401 has no error code invalid_signature: $HOOK_BODY"
	# No signature.
	hook_deliver ping "of-local-w2b-$(rand_token)" "$WORK/ping.json" none
	[ "$HOOK_CODE" = 401 ] || fail "a delivery without a signature got HTTP $HOOK_CODE, not 401"
	# A good signature of another body (the body was changed after it was signed).
	_w2_good=$(hook_sign "$(hook_secret)" "$WORK/ping.json")
	jq '.zen = "changed after signing"' "$WORK/ping.json" >"$WORK/ping-changed.json"
	hook_deliver ping "of-local-w2c-$(rand_token)" "$WORK/ping-changed.json" "$_w2_good"
	[ "$HOOK_CODE" = 401 ] || fail "a delivery with a body that does not match its signature got HTTP $HOOK_CODE, not 401"
	# The same body with its own signature is accepted: the 401 is about the signature.
	hook_deliver ping "of-local-w2d-$(rand_token)" "$WORK/ping.json"
	[ "$HOOK_CODE" = 200 ] || fail "the control delivery with a good signature got HTTP $HOOK_CODE, not 200"
	evidence "wrong signature, no signature and a changed body get 401 invalid_signature. The same body with a good signature gets 200"
	pass
}

# ---------- W3 ----------

# fx_webhook_integration  A GitHub team record that routes the signed
# deliveries of the checks (organization of-local-checks) to the team. A forge
# record needs a credential. The record uses the team
# secret GITHUB_TOKEN when it exists. Else it uses LOCAL_CHECK_TOKEN, a
# placeholder: the checks make no GitHub call that needs it. The record
# checks the signatures with the team secret HOOK_SECRET_REF.
fx_webhook_integration() {
	fx_hook_secret
	_wi_ref=GITHUB_TOKEN
	api_call GET "/secrets/$_wi_ref"
	if [ "$API_CODE" != 200 ]; then
		_wi_ref=LOCAL_CHECK_TOKEN
		api_call GET "/secrets/$_wi_ref"
		if [ "$API_CODE" != 200 ]; then
			api_ok POST /secrets "$(jq -nc --arg n "$_wi_ref" --arg v "of-local-placeholder-$(rand_token)" '{name: $n, value: $v}')"
			log "made the placeholder team secret $_wi_ref"
		fi
	fi
	api_ok GET "/integrations?provider=github"
	_wi_rec=$(printf '%s' "$API_BODY" | jq -c --arg o "$WEBHOOK_OWNER" '[.items[]? | select((.config.org // "" | ascii_downcase) == ($o | ascii_downcase) and (.factory_id // "") == "")][0] // empty')
	if [ -z "$_wi_rec" ]; then
		api_ok POST /integrations "$(jq -nc --arg o "$WEBHOOK_OWNER" --arg r "$_wi_ref" --arg w "$HOOK_SECRET_REF" '{provider: "github", name: "of-local-check", secret_ref: $r, config: {org: $o, webhook_secret_ref: $w}}')"
		log "made the GitHub integration record for the organization $WEBHOOK_OWNER (token $_wi_ref)"
		return 0
	fi
	if [ -z "$(printf '%s' "$_wi_rec" | jq -r '.secret_ref // empty')" ]; then
		# An older record can have no credential.
		api_ok PATCH "/integrations/$(printf '%s' "$_wi_rec" | jq -r .id)" "$(jq -nc --arg r "$_wi_ref" '{secret_ref: $r}')"
		log "gave the GitHub integration record for $WEBHOOK_OWNER the token $_wi_ref"
	fi
	fx_record_hook_secret "$_wi_rec"
}

check_W3() {
	need_cmd curl jq openssl
	fx_check_factory
	fx_webhook_integration
	_w3_tok=$(rand_token)
	_w3_issue=$(($(now) % 900000 + 100000))
	_w3_title="W3 $_w3_tok"
	jq --arg r "$WEBHOOK_OWNER/$WEBHOOK_REPO" --arg t "$_w3_title" --argjson n "$_w3_issue" --arg a "$CHECK_FACTORY_ALIAS" \
		'.repository.full_name = $r | .repository.html_url = "https://github.com/" + $r
		| .issue.number = $n | .issue.title = $t | .issue.html_url = "https://github.com/" + $r + "/issues/" + ($n | tostring)
		| .label.name = "factory:" + $a | .issue.labels = [{name: ("factory:" + $a)}]' "$HERE/webhooks/issues-labeled.json" >"$WORK/w3.json"
	_w3_did="of-local-w3-$_w3_tok"
	hook_deliver issues "$_w3_did" "$WORK/w3.json"
	[ "$HOOK_CODE" = 202 ] || fail "the first delivery got HTTP $HOOK_CODE, not 202: $(printf '%s' "$HOOK_BODY" | cut -c1-200)"
	# A work item and its root run start.
	_w3_wid=
	_w3_t0=$(now)
	while [ $(($(now) - _w3_t0)) -lt 90 ]; do
		api_call GET "/factories/$FACTORY_UID/work-items?search=$(urlenc "$_w3_tok")"
		_w3_wid=$(printf '%s' "$API_BODY" | jq -r --arg t "$_w3_tok" '[.items[]? | select(.title | contains($t))][0].id // empty')
		[ -z "$_w3_wid" ] || break
		sleep 2
	done
	[ -n "$_w3_wid" ] || fail "the first delivery made no work item (search '$_w3_tok') in 90 seconds. Server log: $(server_logs 2>/dev/null | grep -i -E 'github|webhook|dispatch' | tail -n 3 | cut -c1-200 | tr '\n' '|')"
	api_ok GET "/work-items/$_w3_wid"
	_w3_run=$(printf '%s' "$API_BODY" | jq -r '.root_run_id // (.runs[0].run_id // .runs[0].id) // empty')
	[ -n "$_w3_run" ] || fail "the work item $_w3_wid has no root run"
	printf '%s\n' "$_w3_run" >>"$STATE_DIR/runs"
	evidence "the first delivery made the work item $_w3_wid and the run $_w3_run"
	expect_reply "$_w3_run" 0 assistant 'WEBHOOK-READY' 300 "the first turn"
	_w3_seq=$(last_seq "$_w3_run")
	api_call GET "/runs?work_item_id=$_w3_wid&limit=100"
	_w3_runs=$(printf '%s' "$API_BODY" | jq '.items | length')
	api_call GET "/runs/$_w3_run/inputs"
	_w3_inputs=$(printf '%s' "$API_BODY" | jq '.items | length' 2>/dev/null || echo 0)
	log "before the repeat: $_w3_runs run(s), $_w3_inputs input(s), last message $_w3_seq"
	# The same delivery again, several times (the replicas have separate memories).
	_w3_dup=0
	_w3_i=0
	while [ "$_w3_i" -lt 6 ]; do
		_w3_i=$((_w3_i + 1))
		hook_deliver issues "$_w3_did" "$WORK/w3.json"
		case $HOOK_CODE in
		200) printf '%s' "$HOOK_BODY" | grep -q duplicate && _w3_dup=$((_w3_dup + 1)) ;;
		202) ;;
		*) fail "a repeated delivery got HTTP $HOOK_CODE" ;;
		esac
	done
	[ "$_w3_dup" -ge 1 ] || fail "none of 6 repeated deliveries was answered as a duplicate (200 {\"status\":\"duplicate\"})"
	sleep 25
	api_ok GET "/factories/$FACTORY_UID/work-items?search=$(urlenc "$_w3_tok")"
	[ "$(printf '%s' "$API_BODY" | jq --arg t "$_w3_tok" '[.items[]? | select(.title | contains($t))] | length')" -eq 1 ] || fail "the repeated deliveries made more than one work item"
	api_ok GET "/runs?work_item_id=$_w3_wid&limit=100"
	_w3_runs2=$(printf '%s' "$API_BODY" | jq '.items | length')
	[ "$_w3_runs2" = "$_w3_runs" ] || fail "the work item has $_w3_runs2 run(s) after the repeat, was $_w3_runs"
	api_ok GET "/runs/$_w3_run/inputs"
	_w3_inputs2=$(printf '%s' "$API_BODY" | jq '.items | length' 2>/dev/null || echo 0)
	[ "$_w3_inputs2" = "$_w3_inputs" ] || fail "the run has $_w3_inputs2 input(s) after the repeat, was $_w3_inputs"
	[ "$(last_seq "$_w3_run")" = "$_w3_seq" ] || fail "the run got new messages after the repeat (a second turn)"
	evidence "6 repeated deliveries ($_w3_dup answered 200 duplicate) started no second work item, run, input or turn"
	pass
}

# ---------- G checks (GitHub) ----------

GH_API=https://api.github.com

# gh_call METHOD PATH [BODY]  Call the GitHub REST API with GITHUB_TOKEN. It sets GH_CODE and GH_BODY.
gh_call() {
	_gc_m=$1
	_gc_p=$2
	_gc_b=${3:-}
	if [ -n "$_gc_b" ]; then
		printf '%s' "$_gc_b" >"$WORK/gh.in"
		set -- -H 'Content-Type: application/json' --data-binary @"$WORK/gh.in"
	else
		set --
	fi
	GH_CODE=$(printf 'header = "Authorization: Bearer %s"\nheader = "Accept: application/vnd.github+json"\nheader = "X-GitHub-Api-Version: 2022-11-28"\n' "$GITHUB_TOKEN" |
		curl -sS -m 40 -K - -X "$_gc_m" "$@" -o "$WORK/gh.out" -w '%{http_code}' "$GH_API$_gc_p" 2>>"$CHECK_LOG") || GH_CODE=000
	GH_BODY=$(cat "$WORK/gh.out" 2>/dev/null || true)
}

gh_ok() {
	gh_call "$@"
	case $GH_CODE in
	2??) ;;
	*) fail "GitHub $1 $2 answered $GH_CODE: $(printf '%s' "$GH_BODY" | jq -r '.message // empty' 2>/dev/null | cut -c1-200)" ;;
	esac
}

# gh_needs  Skip unless the settings and the environment allow the GitHub checks.
gh_needs() {
	[ "$LOCAL_FORGE" = github ] || skip "needs LOCAL_FORGE=github (it is $LOCAL_FORGE)"
	[ -n "${LOCAL_GITHUB_REPO:-}" ] || skip "LOCAL_GITHUB_REPO is not set in the environment of check.sh"
	[ -n "${GITHUB_TOKEN:-}" ] || skip "GITHUB_TOKEN is not set in the environment of check.sh (the checks read the repository with it)"
	case $LOCAL_GITHUB_REPO in */*) ;; *) fail "LOCAL_GITHUB_REPO must be owner/name, not '$LOCAL_GITHUB_REPO'" ;; esac
	need_cmd curl jq openssl base64
	GH_OWNER=${LOCAL_GITHUB_REPO%%/*}
}

# fx_github  The server side of the GitHub checks: the team secret, the
# integration record and the factory of-local-gh. It sets GH_FACTORY and GH_ALIAS.
fx_github() {
	gh_ok GET "/repos/$LOCAL_GITHUB_REPO"
	GH_DEFAULT_BRANCH=$(printf '%s' "$GH_BODY" | jq -r '.default_branch')
	printf '%s' "$GH_BODY" | jq -e '.permissions.push == true' >/dev/null || fail "the token of the environment cannot push to $LOCAL_GITHUB_REPO"
	printf '%s' "$GH_BODY" | jq -c '{id, full_name, html_url, default_branch}' >"$STATE_DIR/gh-repo.json"
	api_call GET /secrets/GITHUB_TOKEN
	[ "$API_CODE" = 200 ] || fail "the team secret GITHUB_TOKEN does not exist on the server (openfactory secret set GITHUB_TOKEN)"
	fx_hook_secret
	api_ok GET "/integrations?provider=github"
	# One team record for each owner: a second one gets 409.
	_fg_rec=$(printf '%s' "$API_BODY" | jq -c --arg o "$GH_OWNER" '[.items[]? | select((.config.org // "" | ascii_downcase) == ($o | ascii_downcase) and (.factory_id // "") == "")][0] // empty')
	if [ -z "$_fg_rec" ]; then
		api_ok POST /integrations "$(jq -nc --arg o "$GH_OWNER" --arg w "$HOOK_SECRET_REF" '{provider: "github", name: "of-local-github", secret_ref: "GITHUB_TOKEN", config: {org: $o, webhook_secret_ref: $w}}')"
		log "made the GitHub integration record for $GH_OWNER (token GITHUB_TOKEN)"
	else
		if [ "$(printf '%s' "$_fg_rec" | jq -r '.secret_ref // empty')" != GITHUB_TOKEN ]; then
			api_ok PATCH "/integrations/$(printf '%s' "$_fg_rec" | jq -r .id)" '{"secret_ref": "GITHUB_TOKEN"}'
			log "set the token of the GitHub integration record for $GH_OWNER to GITHUB_TOKEN"
		fi
		fx_record_hook_secret "$_fg_rec"
	fi
	GH_ALIAS=of-local-gh
	fx_factory "$HERE/factories/gh" "$GH_ALIAS"
	GH_FACTORY=$FACTORY_UID
}

# g_ensure_pr  Make one issue and the pull request that the factory opens for it.
# It sets G_TOK, G_ISSUE, G_PR, G_BRANCH, G_WID, G_RUN. The state is kept for G2 to G4 in this run.
g_ensure_pr() {
	if [ -s "$STATE_DIR/g_pr" ]; then
		# shellcheck source=/dev/null
		. "$STATE_DIR/g_pr"
		return 0
	fi
	fx_github
	G_TOK=$(rand_token)
	G_BRANCH=of-local-g1-$G_TOK
	gh_call POST "/repos/$LOCAL_GITHUB_REPO/labels" "$(jq -nc --arg n "factory:$GH_ALIAS" '{name: $n, color: "0e8a16"}')"
	_gp_cmd="git checkout -q -b $G_BRANCH && echo g1-$G_TOK > g1-$G_TOK.txt && git add g1-$G_TOK.txt && git commit -q -m 'of-local G1 $G_TOK' && git push -q origin $G_BRANCH && gh pr create --title 'of-local G1 $G_TOK' --body 'G1 $G_TOK' --head $G_BRANCH --base $GH_DEFAULT_BRANCH && echo G1-PR-OPENED-$G_TOK"
	_gp_body="Check G1 $G_TOK. $(tool_directive bash "$(jq -nc --arg c "$_gp_cmd" '{command: $c, timeout_seconds: 300}')")"
	gh_ok POST "/repos/$LOCAL_GITHUB_REPO/issues" "$(jq -nc --arg t "of-local G1 $G_TOK" --arg b "$_gp_body" --arg l "factory:$GH_ALIAS" '{title: $t, body: $b, labels: [$l]}')"
	G_ISSUE=$(printf '%s' "$GH_BODY" | jq -r '.number')
	log "made the issue #$G_ISSUE in $LOCAL_GITHUB_REPO"
	gh_ok GET "/repos/$LOCAL_GITHUB_REPO/issues/$G_ISSUE"
	printf '%s' "$GH_BODY" >"$WORK/gh-src.json"
	jq -c --slurpfile repo "$STATE_DIR/gh-repo.json" --argjson bot "$BOT_JSON" --arg alias "$GH_ALIAS" \
		'{action: "labeled", label: {name: ("factory:" + $alias)}, issue: ., repository: $repo[0], sender: $bot}' "$WORK/gh-src.json" >"$WORK/gh-delivery.json" || fail "cannot build the issues delivery"
	hook_deliver issues "of-local-g1b-$G_TOK" "$WORK/gh-delivery.json"
	[ "$HOOK_CODE" = 202 ] || fail "the signed issues delivery got HTTP $HOOK_CODE: $(printf '%s' "$HOOK_BODY" | cut -c1-200)"
	# The work item of the issue.
	G_WID=
	_gp_t0=$(now)
	while [ $(($(now) - _gp_t0)) -lt 120 ]; do
		api_call GET "/factories/$GH_FACTORY/work-items?search=$(urlenc "$G_TOK")"
		G_WID=$(printf '%s' "$API_BODY" | jq -r --arg t "$G_TOK" '[.items[]? | select(.title | contains($t))][0].id // empty')
		[ -z "$G_WID" ] || break
		sleep 3
	done
	[ -n "$G_WID" ] || fail "the labeled issue made no work item in the factory $GH_ALIAS"
	api_ok GET "/work-items/$G_WID"
	G_RUN=$(printf '%s' "$API_BODY" | jq -r '.root_run_id // empty')
	printf '%s\n' "$G_RUN" >>"$STATE_DIR/runs"
	# The pull request that the agent opens.
	G_PR=
	_gp_t0=$(now)
	while [ $(($(now) - _gp_t0)) -lt 420 ]; do
		gh_call GET "/repos/$LOCAL_GITHUB_REPO/pulls?head=$GH_OWNER:$G_BRANCH&state=all"
		G_PR=$(printf '%s' "$GH_BODY" | jq -r '.[0].number // empty' 2>/dev/null)
		[ -z "$G_PR" ] || break
		run_get "$G_RUN"
		case $RUN_STATE in FAILED | ERROR | CANCELLED) fail "the run $G_RUN ended before a pull request existed ($(run_why "$G_RUN"))" ;; esac
		sleep 5
	done
	[ -n "$G_PR" ] || fail "no pull request for the branch $G_BRANCH in $LOCAL_GITHUB_REPO after 420 seconds"
	{
		printf 'G_TOK=%s\nG_ISSUE=%s\nG_PR=%s\nG_BRANCH=%s\nG_WID=%s\nG_RUN=%s\nGH_ALIAS=%s\nGH_FACTORY=%s\nGH_DEFAULT_BRANCH=%s\n' \
			"$G_TOK" "$G_ISSUE" "$G_PR" "$G_BRANCH" "$G_WID" "$G_RUN" "$GH_ALIAS" "$GH_FACTORY" "$GH_DEFAULT_BRANCH"
	} >"$STATE_DIR/g_pr"
}

check_G1() {
	gh_needs
	g_ensure_pr
	gh_ok GET "/repos/$LOCAL_GITHUB_REPO/pulls/$G_PR"
	evidence "the issue #$G_ISSUE with the label factory:$GH_ALIAS made the pull request #$G_PR ($(printf '%s' "$GH_BODY" | jq -r '.html_url')) on the branch $G_BRANCH"
	pass
}

check_G2() {
	gh_needs
	g_ensure_pr
	# Link the pull request to the work item, as the foreman does with link_pull_request.
	gh_ok GET "/repos/$LOCAL_GITHUB_REPO/pulls/$G_PR"
	printf '%s' "$GH_BODY" >"$WORK/gh-pr.json"
	_g2_url=$(jq -r '.html_url' "$WORK/gh-pr.json")
	_g2_sha=$(jq -r '.head.sha' "$WORK/gh-pr.json")
	_g2_after=$(last_seq "$G_RUN")
	_g2_link=$(tool_directive mcp__factory__link_pull_request "$(jq -nc --arg u "$_g2_url" --arg r "$LOCAL_GITHUB_REPO" --argjson n "$G_PR" --arg b "$G_BRANCH" '{url: $u, repo: $r, number: $n, branch: $b, forge: "github"}')")
	api_ok POST "/work-items/$G_WID/messages" "$(jq -nc --arg m "G2 link the pull request. $_g2_link" '{message: $m}')"
	expect_reply "$G_RUN" "$_g2_after" assistant 'DONE' 240 "the link of the pull request"
	api_ok GET "/work-items/$G_WID"
	printf '%s' "$API_BODY" | jq -e --argjson n "$G_PR" '[.pull_requests[]? | select(.number == $n)] | length > 0' >/dev/null ||
		fail "the work item $G_WID does not list the pull request #$G_PR after link_pull_request"
	# A review comment asks for a follow-up commit and a reply.
	_g2_cmd="git fetch -q origin $G_BRANCH && git checkout -q $G_BRANCH && echo g2-$G_TOK >> g1-$G_TOK.txt && git commit -q -am 'of-local G2 $G_TOK' && git push -q origin $G_BRANCH && echo G2-PUSHED-$G_TOK"
	_g2_reply=$(tool_directive mcp__factory__post_update "$(jq -nc --arg t "G2 reply $G_TOK" '{text: $t}')")
	_g2_text="Please add a line. $(tool_directive bash "$(jq -nc --arg c "$_g2_cmd" '{command: $c, timeout_seconds: 300}')") $_g2_reply"
	gh_ok POST "/repos/$LOCAL_GITHUB_REPO/pulls/$G_PR/comments" "$(jq -nc --arg b "$_g2_text" --arg s "$_g2_sha" --arg p "g1-$G_TOK.txt" '{body: $b, commit_id: $s, path: $p, line: 1, side: "RIGHT"}')"
	_g2_cid=$(printf '%s' "$GH_BODY" | jq -r '.id')
	printf '%s' "$GH_BODY" >"$WORK/gh-src.json"
	_g2_commits_before=$(gh_call GET "/repos/$LOCAL_GITHUB_REPO/commits?sha=$G_BRANCH&per_page=100" && printf '%s' "$GH_BODY" | jq 'length')
	_g2_after=$(last_seq "$G_RUN")
	jq -c --slurpfile pr "$WORK/gh-pr.json" --slurpfile repo "$STATE_DIR/gh-repo.json" --argjson bot "$BOT_JSON" \
		'{action: "created", comment: ., pull_request: $pr[0], repository: $repo[0], sender: $bot}' "$WORK/gh-src.json" >"$WORK/gh-delivery.json" || fail "cannot build the review comment delivery"
	hook_deliver pull_request_review_comment "of-local-g2-$G_TOK" "$WORK/gh-delivery.json"
	[ "$HOOK_CODE" = 202 ] || fail "the review comment delivery got HTTP $HOOK_CODE: $(printf '%s' "$HOOK_BODY" | cut -c1-200)"
	# A follow-up commit on the branch, and a reply in the review thread.
	_g2_commit=
	_g2_replied=
	_g2_t0=$(now)
	while [ $(($(now) - _g2_t0)) -lt 420 ]; do
		gh_call GET "/repos/$LOCAL_GITHUB_REPO/commits?sha=$G_BRANCH&per_page=100"
		[ "$(printf '%s' "$GH_BODY" | jq 'length' 2>/dev/null)" -gt "${_g2_commits_before:-0}" ] 2>/dev/null && _g2_commit=1
		gh_call GET "/repos/$LOCAL_GITHUB_REPO/pulls/$G_PR/comments?per_page=100"
		printf '%s' "$GH_BODY" | jq -e --argjson c "$_g2_cid" '[.[] | select(.in_reply_to_id == $c)] | length > 0' >/dev/null 2>&1 && _g2_replied=1
		[ -n "$_g2_commit" ] && [ -n "$_g2_replied" ] && break
		sleep 5
	done
	[ -n "$_g2_commit" ] || fail "the branch $G_BRANCH got no follow-up commit after the review comment"
	[ -n "$_g2_replied" ] || fail "the review thread of comment $_g2_cid got no reply"
	evidence "the review comment $_g2_cid made a follow-up commit on $G_BRANCH and a reply in its thread"
	pass
}

check_G3() {
	gh_needs
	g_ensure_pr
	gh_ok GET "/repos/$LOCAL_GITHUB_REPO/pulls/$G_PR"
	# The text of the merge event goes to the foreman: it carries the call that completes the work item.
	_g3_done=$(tool_directive mcp__factory__complete_work_item "$(jq -nc --arg n "merged $G_TOK" '{note: $n}')")
	gh_ok PATCH "/repos/$LOCAL_GITHUB_REPO/pulls/$G_PR" "$(jq -nc --arg b "G3 $G_TOK. $_g3_done" '{body: $b}')"
	gh_ok PUT "/repos/$LOCAL_GITHUB_REPO/pulls/$G_PR/merge" '{"merge_method":"squash"}'
	gh_ok GET "/repos/$LOCAL_GITHUB_REPO/pulls/$G_PR"
	printf '%s' "$GH_BODY" >"$WORK/gh-src.json"
	printf '%s' "$GH_BODY" | jq -e '.merged == true' >/dev/null || fail "GitHub does not show the pull request #$G_PR as merged"
	jq -c --slurpfile repo "$STATE_DIR/gh-repo.json" --argjson bot "$BOT_JSON" \
		'{action: "closed", pull_request: ., repository: $repo[0], sender: $bot}' "$WORK/gh-src.json" >"$WORK/gh-delivery.json" || fail "cannot build the pull request delivery"
	hook_deliver pull_request "of-local-g3-$G_TOK" "$WORK/gh-delivery.json"
	[ "$HOOK_CODE" = 202 ] || fail "the merge delivery got HTTP $HOOK_CODE: $(printf '%s' "$HOOK_BODY" | cut -c1-200)"
	_g3_stage=
	_g3_t0=$(now)
	while [ $(($(now) - _g3_t0)) -lt 300 ]; do
		api_call GET "/work-items/$G_WID"
		_g3_stage=$(printf '%s' "$API_BODY" | jq -r '.stage // empty')
		[ "$_g3_stage" != complete ] || break
		sleep 4
	done
	[ "$_g3_stage" = complete ] || fail "the work item $G_WID is in the stage '$_g3_stage', not complete, 300 seconds after the merge"
	evidence "the merge of #$G_PR closed the work item $G_WID (stage complete)"
	pass
}

check_G4() {
	gh_needs
	fx_github
	_g4_tok=$(rand_token)
	_g4_dir=of-local-definition
	_g4_src=$HERE/factories/gh-definition
	# 1. The definition files are on the default branch.
	for _g4_f in factory.yaml agents/foreman/agent.md; do
		gh_call GET "/repos/$LOCAL_GITHUB_REPO/contents/$_g4_dir/$_g4_f?ref=$GH_DEFAULT_BRANCH"
		if [ "$GH_CODE" = 404 ]; then
			gh_ok PUT "/repos/$LOCAL_GITHUB_REPO/contents/$_g4_dir/$_g4_f" "$(jq -nc --arg m "of-local checks: definition file $_g4_f" --arg b "$GH_DEFAULT_BRANCH" --arg c "$(base64 <"$_g4_src/$_g4_f" | tr -d '\n')" '{message: $m, content: $c, branch: $b}')"
			log "added $_g4_dir/$_g4_f to $GH_DEFAULT_BRANCH"
		fi
	done
	# 2. The factory with a git definition source.
	_g4_uid=$(factory_uid of-local-gh-def)
	if [ -z "$_g4_uid" ]; then
		api_ok POST /factories "$(jq -nc --arg r "$LOCAL_GITHUB_REPO" --arg b "$GH_DEFAULT_BRANCH" --arg p "$_g4_dir" '{name: "of-local-gh-def", alias: "of-local-gh-def", source: "git", git_repo: $r, git_branch: $b, git_path: $p}')"
		log "made the factory of-local-gh-def with the definition source $LOCAL_GITHUB_REPO:$_g4_dir"
	fi
	# 3. A pull request that changes the definition.
	_g4_branch=of-local-g4-$_g4_tok
	gh_ok GET "/repos/$LOCAL_GITHUB_REPO/git/ref/heads/$GH_DEFAULT_BRANCH"
	_g4_base=$(printf '%s' "$GH_BODY" | jq -r '.object.sha')
	gh_ok POST "/repos/$LOCAL_GITHUB_REPO/git/refs" "$(jq -nc --arg r "refs/heads/$_g4_branch" --arg s "$_g4_base" '{ref: $r, sha: $s}')"
	gh_ok GET "/repos/$LOCAL_GITHUB_REPO/contents/$_g4_dir/agents/foreman/agent.md?ref=$_g4_branch"
	_g4_fsha=$(printf '%s' "$GH_BODY" | jq -r '.sha')
	sed "s/G4-MARKER: main/G4-MARKER: $_g4_tok/" "$_g4_src/agents/foreman/agent.md" >"$WORK/g4-agent.md"
	gh_ok PUT "/repos/$LOCAL_GITHUB_REPO/contents/$_g4_dir/agents/foreman/agent.md" "$(jq -nc --arg b "$_g4_branch" --arg sha "$_g4_fsha" --arg c "$(base64 <"$WORK/g4-agent.md" | tr -d '\n')" '{message: "of-local G4: change the foreman", content: $c, branch: $b, sha: $sha}')"
	gh_ok POST "/repos/$LOCAL_GITHUB_REPO/pulls" "$(jq -nc --arg t "of-local G4 $_g4_tok" --arg h "$_g4_branch" --arg b "$GH_DEFAULT_BRANCH" '{title: $t, head: $h, base: $b, body: "Check G4"}')"
	_g4_pr=$(printf '%s' "$GH_BODY" | jq -r '.number')
	_g4_head=$(printf '%s' "$GH_BODY" | jq -r '.head.sha')
	printf '%s' "$GH_BODY" >"$WORK/gh-src.json"
	log "pull request #$_g4_pr, head $_g4_head"
	jq -c --slurpfile repo "$STATE_DIR/gh-repo.json" --argjson bot "$BOT_JSON" '{action: "opened", pull_request: ., repository: $repo[0], sender: $bot}' "$WORK/gh-src.json" >"$WORK/gh-delivery.json" ||
		fail "cannot build the pull request delivery"
	hook_deliver pull_request "of-local-g4-$_g4_tok" "$WORK/gh-delivery.json"
	[ "$HOOK_CODE" = 202 ] || fail "the pull request delivery got HTTP $HOOK_CODE: $(printf '%s' "$HOOK_BODY" | cut -c1-200)"
	# 4. The status of the check on the head commit.
	_g4_status=
	_g4_t0=$(now)
	while [ $(($(now) - _g4_t0)) -lt 180 ]; do
		gh_call GET "/repos/$LOCAL_GITHUB_REPO/commits/$_g4_head/statuses"
		_g4_status=$(printf '%s' "$GH_BODY" | jq -r '[.[]? | select(.context | startswith("openfactory/definition"))][0] | "\(.context) \(.state) \(.description)"' 2>/dev/null)
		case $_g4_status in "openfactory/definition"*) break ;; esac
		_g4_status=
		gh_call GET "/repos/$LOCAL_GITHUB_REPO/commits/$_g4_head/check-runs"
		_g4_status=$(printf '%s' "$GH_BODY" | jq -r '[.check_runs[]? | select(.name | startswith("openfactory/definition"))][0] | "\(.name) \(.conclusion) \(.output.title // "")"' 2>/dev/null)
		case $_g4_status in "openfactory/definition"*) break ;; esac
		_g4_status=
		sleep 5
	done
	gh_call PATCH "/repos/$LOCAL_GITHUB_REPO/pulls/$_g4_pr" '{"state":"closed"}'
	gh_call DELETE "/repos/$LOCAL_GITHUB_REPO/git/refs/heads/$_g4_branch"
	[ -n "$_g4_status" ] || fail "the head commit of the pull request #$_g4_pr has no status or check run 'openfactory/definition' 180 seconds after the delivery"
	case $_g4_status in *success* | *"No factory"*) ;; *) fail "the definition check of #$_g4_pr is not successful: $_g4_status" ;; esac
	evidence "the pull request #$_g4_pr that changes the definition got: $_g4_status"
	pass
}
