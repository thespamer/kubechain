# KubeChain

**Cryptographic consensus for Kubernetes configuration changes.**  
Hyperledger Fabric · SmartBFT · Kubebuilder · AI validation

---

## The problem this solves

In production Kubernetes clusters, any engineer with access to `kubectl` can apply a `NetworkPolicy`, `ClusterRole`, or `Deployment` change without anyone, whether a tool or a process, formally agreeing to it. GitOps helps because changes are tracked in git, but the model is still based on **trust**: you trust that the process was followed, that the PR was reviewed, and that nobody will run a direct `kubectl apply`.

KubeChain removes trust as a requirement. It replaces trust with **proof**.

Every configuration change must:

1. Be analyzed by an **AI Validator** for semantic consistency between the manifest and the changelog
2. Be **endorsed by multiple organizations** in a Hyperledger Fabric network
3. Have consensus confirmed by the **SmartBFT algorithm** on the orderers
4. Generate a **ProofToken**, a cryptographic hash of the block in the ledger
5. Present that token to the **ValidatingAdmissionWebhook** at apply time

No token. No ledger block. No apply. There is no bypass.

---

## Architectural inspiration: Chainlink "Proof over Trust"

Chainlink built infrastructure where external data is cryptographically verified before reaching smart contracts, without relying on trust in whoever supplied the data. Truth is proven, not promised.

KubeChain translates that principle to the Kubernetes control plane:

| Chainlink | KubeChain |
|-----------|-----------|
| Proof of Reserve | Proof of Config — cluster state recorded in the ledger |
| Decentralized Oracle Network | Multi-org peers (Infra, Sec, SRE, Compliance) |
| Smart contract onchain | KubeGov Chaincode — validates, records, and emits proofs |
| Cryptographic truth | ProofToken — SHA-256(proposalID + txID + manifestHash) |
| Cannot trust, must verify | Webhook blocks without proof — fail-closed by default |

---

## How it works

### Flow overview

```
Engineer                 KubeChain Operator        Hyperledger Fabric
   │                           │                          │
   │── kubectl apply CCR ────> │                          │
   │   (ConfigChangeRequest)   │                          │
   │                           │─── AI Validate diff ─── │
   │                           │    (LLM: changelog vs    │
   │                           │     manifest semantics)  │
   │                           │                          │
   │                           │─── SubmitProposal ─────> │
   │                           │                          │── ProposeChange()
   │                           │                          │── Endorse (Infra+Sec+SRE)
   │                           │                          │── SmartBFT consensus
   │                           │                          │── Block committed
   │                           │                          │
   │                           │ <── ProofToken (kt_…) ──│
   │                           │                          │
   │                           │─── kubectl apply ──────> kube-apiserver
   │                           │    manifest +            │
   │                           │    annotation proofToken │
   │                           │                          │
   │                    ValidatingWebhook                 │
   │                           │─── QueryTx(token) ─────> │
   │                           │ <── Valid: true ─────── │
   │                           │                          │
   │                           │─── Allowed ────────────> etcd
   │                           │                          │
   │ <── CCR status: Applied ──│                          │


If someone tries a direct kubectl apply without a CCR:
   │── kubectl apply ─────────> ValidatingWebhook
   │                            │── no proof-token
   │                            │── 403 Forbidden
   │ <── "Create a CCR" ───────│
```

### The four main components

#### 1. ConfigChangeRequest (CRD)

The Kubernetes object that represents a change proposal. It is the entry point for the whole system.

```yaml
apiVersion: kubechain.io/v1
kind: ConfigChangeRequest
metadata:
  name: allow-payments-to-database-netpol
  namespace: kubechain-system
spec:
  resourceKind: NetworkPolicy
  resourceName: allow-payments-to-database
  namespace: database
  changelogDescription: >
    Allows TCP 5432 from the payments namespace to database.
    Required for payments-service-v2 migrated in PR #1847.
  prReference: "https://github.com/org/repo/pull/1847"
  requestedBy: "joao.silva@empresa.com"
  urgency: medium
  rawManifest:
    apiVersion: networking.k8s.io/v1
    kind: NetworkPolicy
    # ... full manifest
```

The CCR moves through these states:

```
Pending → AIValidating → FabricPending → Endorsed → Approved → Applied
                                                         ↓
                                                      Rejected (AI score > 0.85)
```

#### 2. Reconciler (Kubernetes Operator)

The Go controller built with Kubebuilder that orchestrates the pipeline. At each state transition it:

- **Pending → AIValidating**: fetches the current resource state from the cluster, builds the diff, and sends it to the AI Validator
- **AIValidating → FabricPending**: if the risk score is acceptable (< 0.85), submits the proposal to Fabric
- **FabricPending → Approved**: waits for the ProofToken from the ledger and writes it to the CCR `.status`
- **Approved → Applied**: injects the ProofToken as an annotation and applies the manifest via Server-Side Apply

Every transition is written to `.status` before advancing. If the pod restarts mid-process, the reconciler resumes exactly where it stopped.

#### 3. KubeGov Chaincode (Hyperledger Fabric)

The smart contract written in Go that runs on each organization's peers. It is the only place where the "proof" is created.

Main functions:

- **`ProposeChange()`** — records the proposal in the ledger with the manifest hash, AI score, changelog, and PR reference
- **`ApproveChange()`** — after SmartBFT consensus, generates the ProofToken: `SHA-256(proposalID + txID + manifestHash)`. This token is mathematically bound to the specific block in the ledger.
- **`VerifyProofToken()`** — called by the webhook on every admission. It verifies: (a) the token exists in the ledger, (b) the token was not invalidated, and (c) the submitted manifest hash matches the approved hash, detecting any post-approval tampering.
- **`MarkApplied()`** — records that the change was actually applied to the cluster
- **`GetProposalHistory()`** — returns the full history of all changes to a resource

The channel **endorse policy** ensures that no proposal is approved without:

```
AND('org-infra', 'org-sec', OR('org-sre', 'org-compliance'))
```

Infra and Security are mandatory, plus at least one third organization.

#### 4. ValidatingAdmissionWebhook

Intercepts every create or update operation on monitored resource types, directly in the kube-apiserver, before any data reaches etcd.

The validation logic has three sequential checks:

1. **Annotation present?** — does `kubechain.io/proof-token` exist on the resource?
2. **Valid token in Fabric?** — does the token exist in the ledger and has it not been invalidated?
3. **Hash matches?** — does the SHA-256 hash of the current manifest match the hash recorded at approval time?

If any check fails → `403 Forbidden`. No exception, no manual override.

**Fail-closed behavior**: if Fabric is temporarily unavailable when the webhook tries to verify the token, admission is denied. Security takes priority over availability.

**Only exception**: the `kubechain-operator` ServiceAccount itself is allowlisted because it injects the token and applies the manifest after approval.

---

## Monitored Kubernetes resources

By default, the webhook intercepts changes to these types:

| Category | Resource types |
|-----------|---------------|
| Network | `NetworkPolicy` |
| RBAC | `ClusterRole`, `ClusterRoleBinding`, `Role`, `RoleBinding` |
| Workloads | `Deployment`, `StatefulSet`, `DaemonSet` |
| Configuration | `ConfigMap` (with label `kubechain.io/sensitive=true`), `Secret` |
| Exposure | `Ingress` |
| Admission | `MutatingWebhookConfiguration`, `ValidatingWebhookConfiguration` |
| Infrastructure | `Namespace` |

Other resource types pass through the kube-apiserver freely without KubeChain intervention.

---

## AI validation

The AI Validator is a sidecar in the operator pod. It receives the diff and changelog, then performs semantic analysis using an LLM.

The prompt is structured to detect:

- **Semantic inconsistency**: the changelog says "label adjustment" but the manifest adds a `ClusterRoleBinding` with `verbs: ["*"]`
- **Privilege escalation**: wildcard verbs or dangerous capabilities (`SYS_ADMIN`, `NET_ADMIN`)
- **Unjustified exposure**: NetworkPolicy that opens traffic not mentioned in the changelog
- **Secret exposure**: env vars or volumeMounts exposing secrets insecurely
- **Webhook tampering**: changes to `ValidatingWebhookConfiguration` or `MutatingWebhookConfiguration`

The LLM returns a **risk score** from 0.0 to 1.0 and a natural-language summary.

| Score | Action |
|-------|------|
| 0.0 – 0.30 | Automatically approved, continues to Fabric |
| 0.30 – 0.60 | Approved with warning, reasoning recorded in status |
| 0.60 – 0.85 | Approved but flagged — requires detailed changelog (≥ 50 chars) |
| 0.85 – 1.0 | **Automatically rejected** — CCR enters `Rejected` |

The sidecar is compatible with any OpenAI-compatible API: local Ollama (`qwen2.5-coder`, `llama3`, `mistral`), OpenAI, Azure OpenAI, or Anthropic Claude.

---

## Security guarantees

| Guarantee | Mechanism |
|----------|-----------|
| Every change has proof | ProofToken cryptographically bound to a Fabric block |
| Nobody applies without consensus | Webhook blocks admission without a valid token |
| Post-approval tampering is detected | Manifest hash is verified in the ledger on every admission |
| Replay attack is impossible | Token is specific to the resource + manifest + proposal |
| A compromised orderer does not break the system | SmartBFT tolerates up to f = ⌊(n-1)/3⌋ malicious orderers |
| The ledger is immutable | Append-only structure with hash-linked blocks |
| Complete audit trail | GetProposalHistory() returns the full history for a resource |
| Fail-closed | Fabric unavailable → admission denied, not allowed |

---

## Project structure

```
kubechain/
│
├── chaincode/
│   └── kubegov.go                  # Fabric smart contract
│                                   # ProposeChange, ApproveChange, VerifyProofToken
│
├── operator/
│   ├── api/v1/
│   │   └── configchangerequest_types.go    # CRD: ConfigChangeRequest + status
│   │
│   ├── controllers/
│   │   └── configchangerequest_controller.go  # Reconciler: state pipeline
│   │
│   ├── webhook/
│   │   └── validating_webhook.go           # Fail-closed admission webhook
│   │
│   └── pkg/
│       ├── fabric/                         # fabric-gateway SDK client
│       │   └── client.go                   # SubmitProposal, VerifyProofToken
│       └── ai/
│           └── validator.go                # AI Validator client (LLM sidecar)
│
├── bevel/
│   └── network.yaml                # Automated Fabric network deployment on K8s
│                                   # 4 orgs, 4 SmartBFT orderers, CouchDB
│
└── docs/
    └── example-ccr-networkpolicy.yaml  # Commented real-world usage example
```

---

## Dependencies

### Runtime (what must exist in the cluster)

| Dependency | Minimum version | Purpose |
|-------------|---------------|------------|
| Kubernetes | 1.28+ | Base platform; support for ValidatingAdmissionPolicy and SSA |
| Hyperledger Fabric | **3.1.4+** | SmartBFT (BFT consensus) — do not use versions < 3.0 (Raft is CFT only) |
| bevel-operator-fabric | 1.9.0+ | Kubernetes operator for managing peers/orderers/CAs as CRDs |
| Vault (HashiCorp) | 1.14+ | Cryptographic key management for peers and orderers |
| CouchDB | 3.3+ | Peer StateDB for rich history queries |
| Istio | 1.20+ | Proxy required by bevel-operator-fabric for mTLS between peers |
| cert-manager | 1.13+ | Automatic TLS for the ValidatingAdmissionWebhook |

### Build (development)

| Dependency | Version | Purpose |
|-------------|--------|------------|
| Go | 1.22+ | Language for the operator, webhook, and chaincode |
| Kubebuilder | 3.x | Operator scaffolding, CRD generation, and webhook manifests |
| controller-gen | 0.14+ | Generates CRD YAML from markers in Go code |
| fabric-gateway | 1.4+ | Go SDK for submitting transactions and querying Fabric |
| fabric-contract-api-go | 2.0+ | API for writing chaincodes in Go |
| Ansible | 2.15+ | Deployment automation via Bevel |
| Helm | 3.x | bevel-operator-fabric charts |
| Docker | 24+ | Builds chaincode and operator images |

### AI Validator (sidecar)

Compatible with any OpenAI-compatible API. Recommended options:

| Option | Suggested model | When to use |
|-------|----------------|-------------|
| Ollama (local) | `qwen2.5-coder:14b` | Air-gapped / compliance / no data leaving the environment |
| OpenAI | `gpt-4o` | Maximum quality, token-based cost |
| Azure OpenAI | `gpt-4o` | Enterprise, data inside the region |
| Anthropic Claude | `claude-sonnet-4-6` | High-quality technical reasoning |

---

## Installation

### Prerequisites

```bash
# Check versions
kubectl version --client
go version              # >= 1.22
helm version            # >= 3.x
ansible --version       # >= 2.15

# Install kubebuilder
curl -L -o kubebuilder "https://go.kubebuilder.io/dl/latest/$(go env GOOS)/$(go env GOARCH)"
chmod +x kubebuilder && sudo mv kubebuilder /usr/local/bin/

# Install cert-manager in the cluster (required by the webhook)
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=120s
```

---

### Step 1 — Deploy the Hyperledger Fabric network (via Bevel)

```bash
# 1.1 Install bevel-operator-fabric in the cluster
kubectl apply -f https://github.com/hyperledger-bevel/bevel-operator-fabric/releases/latest/download/install.yaml
kubectl wait --for=condition=Available deployment --all -n bevel-operator-fabric-system --timeout=180s

# 1.2 Check available Fabric operator CRDs
kubectl get crd | grep hlf

# 1.3 Configure credentials in network.yaml
# Edit bevel/network.yaml with your Vault credentials and K8s contexts

# 1.4 Run the Bevel playbook to create the KubeGov network
# This creates: 4 CAs, 4 peer orgs, 4 SmartBFT orderers, and the kubegov channel
ansible-playbook run.yaml -e "@bevel/network.yaml" -v

# 1.5 Verify that all pods are Running
kubectl get pods -n kubechain-fabric-infra
kubectl get pods -n kubechain-fabric-sec
kubectl get pods -n kubechain-fabric-sre
kubectl get pods -n kubechain-fabric-compliance
kubectl get pods -n kubechain-fabric-orderer

# 1.6 Verify that the kubegov channel was created
# (via peer CLI, inside the peer0-infra pod)
kubectl exec -it -n kubechain-fabric-infra deploy/peer0-infra -- \
  peer channel list
# Expected: kubegov
```

---

### Step 2 — Deploy the KubeGov chaincode

```bash
# 2.1 Build the chaincode
cd chaincode/
go mod tidy
go build ./...

# 2.2 Package it
peer lifecycle chaincode package kubegov.tar.gz \
  --path . \
  --lang golang \
  --label kubegov_1.0

# 2.3 Install on all peers (repeat for each org)
for ORG in infra sec sre compliance; do
  kubectl exec -it -n kubechain-fabric-${ORG} deploy/peer0-${ORG} -- \
    peer lifecycle chaincode install kubegov.tar.gz
done

# 2.4 Approve in each org (requires quorum for the endorse policy)
# Endorse policy: AND(infra, sec, OR(sre, compliance))
PACKAGE_ID=$(kubectl exec -it -n kubechain-fabric-infra deploy/peer0-infra -- \
  peer lifecycle chaincode queryinstalled --output json | jq -r '.installed_chaincodes[0].package_id')

for ORG in infra sec sre; do
  kubectl exec -it -n kubechain-fabric-${ORG} deploy/peer0-${ORG} -- \
    peer lifecycle chaincode approveformyorg \
      --channelID kubegov \
      --name kubegov \
      --version 1.0 \
      --package-id ${PACKAGE_ID} \
      --sequence 1 \
      --signature-policy "AND('org-infra.member','org-sec.member',OR('org-sre.member','org-compliance.member'))"
done

# 2.5 Commit the chaincode on the channel
peer lifecycle chaincode commit \
  --channelID kubegov \
  --name kubegov \
  --version 1.0 \
  --sequence 1 \
  --signature-policy "AND('org-infra.member','org-sec.member',OR('org-sre.member','org-compliance.member'))" \
  --peerAddresses peer0-infra.kubechain-fabric-infra:7051 \
  --peerAddresses peer0-sec.kubechain-fabric-sec:7051 \
  --peerAddresses peer0-sre.kubechain-fabric-sre:7051

# 2.6 Verify active chaincode
peer lifecycle chaincode querycommitted --channelID kubegov
```

---

### Step 3 — Build and deploy the KubeChain Operator

```bash
# 3.1 Initialize the kubebuilder project (generates main.go, Makefile, Dockerfile)
cd operator/
kubebuilder init --domain kubechain.io --repo github.com/your-org/kubechain
kubebuilder create api --group kubechain --version v1 --kind ConfigChangeRequest

# 3.2 Copy this repo's files into the correct locations generated by kubebuilder
# api/v1/configchangerequest_types.go → already exists in the repo
# controllers/configchangerequest_controller.go → already exists in the repo
# webhook/validating_webhook.go → already exists in the repo

# 3.3 Install Go dependencies
go mod tidy
# Main dependencies:
# - sigs.k8s.io/controller-runtime
# - k8s.io/apimachinery
# - github.com/hyperledger/fabric-gateway/pkg/client
# - github.com/hyperledger/fabric-contract-api-go

# 3.4 Generate CRDs and webhook manifests
make manifests
make generate

# 3.5 Install CRDs in the cluster
make install

# 3.6 Build and push the Docker image
make docker-build docker-push IMG=your-registry/kubechain-operator:v1.0.0

# 3.7 Deploy the operator in the cluster
make deploy IMG=your-registry/kubechain-operator:v1.0.0

# 3.8 Verify that the operator is running
kubectl get pods -n kubechain-system
# NAME                                    READY   STATUS    RESTARTS
# kubechain-operator-7d9b4c8f6d-xk2p9   2/2     Running   0
# (2/2 = operator + AI validator sidecar)
```

---

### Step 4 — Configure the AI Validator

```bash
# Option A: local Ollama (air-gapped)
# Add a sidecar with Ollama to the operator deployment

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: kubechain-ai-config
  namespace: kubechain-system
data:
  AI_VALIDATOR_URL: "http://localhost:11434"
  AI_VALIDATOR_MODEL: "qwen2.5-coder:14b-instruct-q4_K_M"
  AI_RISK_THRESHOLD: "0.85"
EOF

# Option B: Claude API
kubectl create secret generic kubechain-ai-secret \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  -n kubechain-system
```

---

### Step 5 — Configure the ValidatingAdmissionWebhook

```bash
# cert-manager creates TLS automatically for the webhook
# kubebuilder generated the base manifest in config/webhook/

# Apply the ValidatingWebhookConfiguration
kubectl apply -f config/webhook/manifests.yaml

# Verify registered webhook
kubectl get validatingwebhookconfigurations
# NAME                          WEBHOOKS   AGE
# kubechain-validating-webhook  1          30s

# Test that the webhook is active (it should block)
kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: test-without-proof
  namespace: default
spec:
  podSelector: {}
EOF
# Expected: Error from server: "KubeChain: change rejected — annotation
#           kubechain.io/proof-token missing. Create a ConfigChangeRequest..."
```

---

### Step 6 — First real change

```bash
# Create a ConfigChangeRequest
kubectl apply -f docs/example-ccr-networkpolicy.yaml

# Follow the pipeline in real time
kubectl get ccr -n kubechain-system -w

# NAME                                PHASE          AISCORE   PROOFTOKEN
# allow-payments-to-database-netpol   Pending
# allow-payments-to-database-netpol   AIValidating
# allow-payments-to-database-netpol   FabricPending
# allow-payments-to-database-netpol   Approved       0.12      kt_7f3a9b2c...
# allow-payments-to-database-netpol   Applied        0.12      kt_7f3a9b2c...

# View full details
kubectl describe ccr allow-payments-to-database-netpol -n kubechain-system

# Verify that the NetworkPolicy was applied with the proof token
kubectl get networkpolicy allow-payments-to-database -n database -o yaml | grep kubechain

# kubechain.io/proof-token: "kt_7f3a9b2c1d4e5f6a..."
# kubechain.io/fabric-txid: "a3f8b2c1d4e5..."
# kubechain.io/ccr-name: "allow-payments-to-database-netpol"
```

---

## Query the audit history

```bash
# Via kubectl — CCR status
kubectl get ccr -A --sort-by=.metadata.creationTimestamp

# Via chaincode query — full history in the Fabric ledger
kubectl exec -it -n kubechain-fabric-infra deploy/peer0-infra -- \
  peer chaincode query \
  --channelID kubegov \
  --name kubegov \
  --ctor '{"function":"GetProposalHistory","Args":["NetworkPolicy","allow-payments-to-database","database"]}'

# Via Grafana (if configured with CouchDB as datasource)
# Dashboard: KubeChain Audit Trail
# Shows: proposals by period, approval/rejection rate,
#        average AI score, distribution by resource kind
```

---

## Endorse policy configuration

The endorse policy defines who must agree to a change. Edit it in `bevel/network.yaml` and reinstall the chaincode.

```
# Default policy (recommended for production):
# Mandatory Infra + Sec + at least 1 of SRE or Compliance
AND('org-infra.member', 'org-sec.member', OR('org-sre.member', 'org-compliance.member'))

# More restrictive policy (critical production changes):
# All 4 teams must agree
AND('org-infra.member', 'org-sec.member', 'org-sre.member', 'org-compliance.member')

# More permissive policy (staging environment):
# Any 2 of 4
OutOf(2, 'org-infra.member', 'org-sec.member', 'org-sre.member', 'org-compliance.member')
```

---

## Fault tolerance (SmartBFT)

With **n orderers**, SmartBFT tolerates **f = ⌊(n-1)/3⌋** malicious orderers simultaneously:

| Orderers | Tolerated failures | Use case |
|----------|-----------------|-------------|
| 4 | 1 | Smaller environment, development |
| 7 | 2 | **Production — recommended** |
| 10 | 3 | High criticality, multiple regions |

Unlike Raft, which is only CFT (Crash Fault Tolerant), SmartBFT tolerates **actively malicious** orderers, not only crashed ones. A compromised orderer attempting to emit false blocks is detected and ignored by the protocol.

---

## Operator environment variables

| Variable | Default | Description |
|----------|--------|-----------|
| `FABRIC_GATEWAY_ENDPOINT` | — | Peer gateway gRPC endpoint, for example `peer0-infra:7051` |
| `FABRIC_CHANNEL_NAME` | `kubegov` | Fabric channel name |
| `FABRIC_CHAINCODE_NAME` | `kubegov` | Chaincode name |
| `FABRIC_MSP_ID` | `org-infra` | MSP ID of the org represented by the operator |
| `FABRIC_TLS_CERT_PATH` | — | Path to the peer TLS certificate, mounted via Vault |
| `AI_VALIDATOR_URL` | `http://localhost:11434` | AI Validator sidecar URL |
| `AI_VALIDATOR_MODEL` | `qwen2.5-coder:14b` | Model to use |
| `AI_RISK_THRESHOLD` | `0.85` | Score above which the proposal is rejected |
| `WEBHOOK_FAIL_CLOSED` | `true` | If `false`, allows when Fabric is unavailable (not recommended) |

---

## What is NOT covered by this MVP

This project is an architectural reference and MVP. For production, add:

- [ ] `pkg/fabric/client.go` — complete fabric-gateway SDK implementation with mTLS, reconnect, and retry
- [ ] `main.go` — generated by kubebuilder, registers controller + webhook in the manager
- [ ] Integration tests with a Fabric network in Kind/Minikube
- [ ] ProofToken revocation policy for change rollbacks
- [ ] Multi-cluster: one KubeChain for multiple K8s clusters sharing the same ledger
- [ ] Grafana dashboard with CouchDB as datasource
- [ ] Zero-Knowledge proof of the AI score, proving score < threshold without revealing the model
- [ ] Integration with Sigstore/Cosign to sign manifests before submitting them to Fabric

---

## References

- [Hyperledger Fabric 3.x + SmartBFT](https://hyperledger-fabric.readthedocs.io/en/release-3.0/)
- [Hyperledger Bevel](https://hyperledger-bevel.readthedocs.io/)
- [bevel-operator-fabric](https://github.com/hyperledger-bevel/bevel-operator-fabric)
- [Kubebuilder Book](https://book.kubebuilder.io/)
- [fabric-gateway Go SDK](https://pkg.go.dev/github.com/hyperledger/fabric-gateway/pkg/client)
- [Chainlink: Cryptographic Truth](https://blog.chain.link/what-is-cryptographic-truth/)
- [ValidatingAdmissionWebhook Kubernetes](https://kubernetes.io/docs/reference/access-authn-authz/admission-controllers/)

---

## License

Apache 2.0 — see `LICENSE`.

*Developed as an architectural reference for enterprise Kubernetes cluster governance.*  
*Not audited for production — use with additional security review.*
