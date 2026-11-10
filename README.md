# selvage

A viewer for API contracts across services. It reads OpenAPI specs and plaintext flow files from a git repo and renders an endpoint reference, sequence diagrams and a compatibility report. A CI gate fails when a spec change breaks a documented flow.

## Why flows matter

oasdiff judges a spec change against "any client". A flow says which consumer calls which operation and which response fields it reads. With that information, selvage judges each change against each real consumer. Two examples:

- Removing an optional response field is `info` in oasdiff. It is breaking for a consumer that reads the field.
- Renaming an `operationId` is `info` in oasdiff. It is breaking for every flow (and every generated client) that uses the old id.

## Repo layout

```
specs/<service>.yaml     one OpenAPI 3.x file per service; "<service>.openapi.yaml" also works
specs/*.schema.json      JSON Schemas a spec $refs (relative paths, resolved at the same revision)
flows/<name>.flow        one flow per file
selvage.yaml             optional verdict rules (below)
```

Flow format:

```
# Checkout
// comments start with //
web -> orders: createOrder {id, status}
orders -> payments: chargeCard {chargeId}
web -> orders: getOrder {status, total.amount}
```

Each step is `consumer -> provider: operationId {fields read from the 2xx JSON response}`. Field paths use dots. Arrays are traversed implicitly.

## Verdict rules

Each oasdiff change gets a verdict for each flow edge it touches. By default the verdict follows oasdiff's level (error -> breaking, warning -> risky, info -> safe). The rules below override that per oasdiff check id:

| Rule | Verdict |
|---|---|
| `breaking`, `risky`, `safe` | That verdict, regardless of oasdiff's level |
| `reads` | Breaking if the consumer reads the property the change names, otherwise safe |
| `oasdiff` | oasdiff's level (use it to turn a default off) |

Built-in defaults:

```yaml
rules:
  api-security-added: breaking                 # callers without the credential get 401s; oasdiff says info
  api-global-security-added: breaking
  response-property-enum-value-added: reads    # only breaks consumers that read the property
```

`selvage.yaml` at the repo root is merged over the defaults. It's read at the head revision, so a rule change goes through review like any spec change.

## Use

```
go run . serve -repo DIR [-addr :8080]
go run . check -repo DIR -base origin/main      # exit 1 on a breaking edge
```

`head` defaults to the working tree. In the UI, set `base`/`head` to any git revision.

## Demo

```
./scripts/demo.sh            # /tmp/selvage-demo: HEAD = example/v1, working tree = example/v2
go run . check -repo /tmp/selvage-demo -base HEAD
go run . serve -repo /tmp/selvage-demo
```

## Stack

Go, `html/template` + htmx, oasdiff (diff and checks, kin-openapi parser), D2 (server-side sequence-diagram SVG). No database: specs reload from disk or git on every request.
