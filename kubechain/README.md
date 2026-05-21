# KubeChain

**Consenso criptográfico para mudanças de configuração no Kubernetes.**  
Hyperledger Fabric · SmartBFT · Kubebuilder · Validação por IA

---

## O problema que isso resolve

Em clusters Kubernetes de produção, qualquer engenheiro com acesso ao `kubectl` pode aplicar uma mudança de `NetworkPolicy`, `ClusterRole` ou `Deployment` sem que ninguém — nem uma ferramenta, nem um processo — tenha concordado formalmente com aquilo. O GitOps ajuda (mudanças rastreadas em git), mas o modelo ainda é baseado em **confiança**: você confia que o processo foi seguido, que o PR foi revisado, que ninguém vai fazer um `kubectl apply` direto.

KubeChain elimina a confiança como requisito. Substitui por **prova**.

Toda mudança de configuração precisa:

1. Ser analisada por um **AI Validator** (consistência semântica entre o manifest e o changelog)
2. Ser **endossada por múltiplas organizações** em uma rede Hyperledger Fabric
3. Ter o consenso confirmado pelo **algoritmo SmartBFT** nos orderers
4. Gerar um **ProofToken** — hash criptográfico do bloco no ledger
5. Apresentar esse token ao **ValidatingAdmissionWebhook** no momento da aplicação

Sem o token. Sem o bloco no ledger. Sem a aplicação. Não existe bypass.

---

## Inspiração arquitetural: Chainlink "Proof over Trust"

O Chainlink construiu uma infraestrutura onde dados externos são verificados criptograficamente antes de chegarem a smart contracts — sem depender de confiar em quem forneceu os dados. A verdade é provada, não prometida.

KubeChain traduz esse princípio para o plano de controle do Kubernetes:

| Chainlink | KubeChain |
|-----------|-----------|
| Proof of Reserve | Proof of Config — estado do cluster registrado no ledger |
| Decentralized Oracle Network | Peers multi-org (Infra, Sec, SRE, Compliance) |
| Smart contract onchain | KubeGov Chaincode — valida, registra, emite provas |
| Cryptographic truth | ProofToken — SHA-256(proposalID + txID + manifestHash) |
| Cannot trust, must verify | Webhook bloqueia sem prova — fail-closed por padrão |

---

## Como funciona

### Visão geral do fluxo

```
Engineer                 KubeChain Operator        Hyperledger Fabric
   │                           │                          │
   │── kubectl apply CCR ────> │                          │
   │   (ConfigChangeRequest)   │                          │
   │                           │─── AI Validate diff ─── │
   │                           │    (LLM: changelog vs    │
   │                           │     manifest semântica)  │
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


Se alguém tentar kubectl apply direto (sem CCR):
   │── kubectl apply ─────────> ValidatingWebhook
   │                            │── sem proof-token
   │                            │── 403 Forbidden
   │ <── "Crie um CCR" ────────│
```

### Os quatro componentes principais

#### 1. ConfigChangeRequest (CRD)

O objeto Kubernetes que representa uma proposta de mudança. É o ponto de entrada de todo o sistema.

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
    Permite TCP 5432 do namespace payments para database.
    Necessário para payments-service-v2 migrado em PR #1847.
  prReference: "https://github.com/org/repo/pull/1847"
  requestedBy: "joao.silva@empresa.com"
  urgency: medium
  rawManifest:
    apiVersion: networking.k8s.io/v1
    kind: NetworkPolicy
    # ... manifest completo
```

O CCR passa pelos seguintes estados:

```
Pending → AIValidating → FabricPending → Endorsed → Approved → Applied
                                                         ↓
                                                      Rejected (AI score > 0.85)
```

#### 2. Reconciler (Kubernetes Operator)

O controller em Go (Kubebuilder) que orquestra o pipeline. A cada transição de estado ele:

- **Pending → AIValidating**: busca o estado atual do resource no cluster, monta o diff e envia para o AI Validator
- **AIValidating → FabricPending**: se o score de risco for aceitável (< 0.85), submete a proposta ao Fabric
- **FabricPending → Approved**: aguarda o ProofToken do ledger e grava no `.status` do CCR
- **Approved → Applied**: injeta o ProofToken como annotation e aplica o manifest via Server-Side Apply

Toda transição é gravada no `.status` antes de avançar — se o pod restartar no meio do processo, o reconciler retoma exatamente de onde parou.

#### 3. KubeGov Chaincode (Hyperledger Fabric)

O smart contract escrito em Go que roda nos peers de cada organização. Ele é o único lugar onde a "prova" é criada.

Funções principais:

- **`ProposeChange()`** — registra a proposta no ledger com hash do manifest, AI score, changelog, e referência ao PR
- **`ApproveChange()`** — após o consenso SmartBFT, gera o ProofToken: `SHA-256(proposalID + txID + manifestHash)`. Este token é matematicamente vinculado ao bloco específico no ledger.
- **`VerifyProofToken()`** — chamado pelo webhook a cada admission. Verifica: (a) o token existe no ledger, (b) o token não foi invalidado, (c) o hash do manifest enviado bate com o hash aprovado — detectando qualquer adulteração pós-aprovação.
- **`MarkApplied()`** — registra que a mudança foi efetivamente aplicada ao cluster
- **`GetProposalHistory()`** — histórico completo de todas as mudanças em um resource

A **endorse policy** do channel garante que nenhuma proposta é aprovada sem:

```
AND('org-infra', 'org-sec', OR('org-sre', 'org-compliance'))
```

Infra + Segurança obrigatórios, mais pelo menos um terceiro.

#### 4. ValidatingAdmissionWebhook

Intercepta toda operação de criação ou update nos resource types monitorados, direto no kube-apiserver, antes de qualquer dado chegar ao etcd.

A lógica de validação tem três verificações em sequência:

1. **Annotation presente?** — `kubechain.io/proof-token` existe no resource?
2. **Token válido no Fabric?** — o token existe no ledger e não foi invalidado?
3. **Hash confere?** — o SHA-256 do manifest atual bate com o hash gravado na aprovação?

Se qualquer verificação falhar → `403 Forbidden`. Sem exceção, sem override manual.

**Comportamento fail-closed**: se o Fabric estiver temporariamente indisponível quando o webhook tenta verificar o token, a admission é negada. Segurança tem prioridade sobre disponibilidade.

**Única exceção**: o próprio `kubechain-operator` ServiceAccount é allowlisted — ele é quem injeta o token e aplica o manifest após aprovação.

---

## Recursos Kubernetes monitorados

Por padrão, o webhook intercepta mudanças nos seguintes tipos:

| Categoria | Resource types |
|-----------|---------------|
| Rede | `NetworkPolicy` |
| RBAC | `ClusterRole`, `ClusterRoleBinding`, `Role`, `RoleBinding` |
| Workloads | `Deployment`, `StatefulSet`, `DaemonSet` |
| Configuração | `ConfigMap` (com label `kubechain.io/sensitive=true`), `Secret` |
| Exposição | `Ingress` |
| Admission | `MutatingWebhookConfiguration`, `ValidatingWebhookConfiguration` |
| Infraestrutura | `Namespace` |

Outros resource types passam livremente pelo kube-apiserver sem intervenção do KubeChain.

---

## Validação por IA

O AI Validator é um sidecar no pod do operator que recebe o diff e o changelog e faz uma análise semântica usando um LLM.

O prompt é estruturado para detectar:

- **Inconsistência semântica**: o changelog diz "ajuste de labels" mas o manifest adiciona um `ClusterRoleBinding` com `verbs: ["*"]`
- **Privilege escalation**: wildcard verbs, capabilities perigosas (`SYS_ADMIN`, `NET_ADMIN`)
- **Exposição não justificada**: NetworkPolicy que abre tráfego não mencionado no changelog
- **Secret exposure**: env vars ou volumeMounts expondo secrets de forma insegura
- **Webhook tampering**: mudanças em `ValidatingWebhookConfiguration` ou `MutatingWebhookConfiguration`

O LLM retorna um **risk score** de 0.0 a 1.0 e um resumo em linguagem natural.

| Score | Ação |
|-------|------|
| 0.0 – 0.30 | Aprovado automaticamente, segue para Fabric |
| 0.30 – 0.60 | Aprovado com aviso, reasoning gravado no status |
| 0.60 – 0.85 | Aprovado mas sinalizado — exige changelog detalhado (≥ 50 chars) |
| 0.85 – 1.0 | **Rejeitado automaticamente** — CCR entra em `Rejected` |

O sidecar é compatível com qualquer API OpenAI-compatible: Ollama local (`qwen2.5-coder`, `llama3`, `mistral`), OpenAI, Azure OpenAI, ou Anthropic Claude.

---

## Garantias de segurança

| Garantia | Mecanismo |
|----------|-----------|
| Toda mudança tem prova | ProofToken vinculado criptograficamente ao bloco no Fabric |
| Ninguém aplica sem consenso | Webhook bloqueia admission sem token válido |
| Adulteração pós-aprovação detectada | Hash do manifest verificado no ledger a cada admission |
| Replay attack impossível | Token é específico para o resource + manifest + proposta |
| Orderer comprometido não quebra | SmartBFT tolera até f = ⌊(n-1)/3⌋ orderers maliciosos |
| Ledger é imutável | Append-only, estrutura de blocos encadeados por hash |
| Auditoria completa | GetProposalHistory() retorna todo histórico de um resource |
| Fail-closed | Fabric indisponível → admission negada (não liberada) |

---

## Estrutura do projeto

```
kubechain/
│
├── chaincode/
│   └── kubegov.go                  # Smart contract Fabric
│                                   # ProposeChange, ApproveChange, VerifyProofToken
│
├── operator/
│   ├── api/v1/
│   │   └── configchangerequest_types.go    # CRD: ConfigChangeRequest + status
│   │
│   ├── controllers/
│   │   └── configchangerequest_controller.go  # Reconciler: pipeline de estados
│   │
│   ├── webhook/
│   │   └── validating_webhook.go           # Admission webhook fail-closed
│   │
│   └── pkg/
│       ├── fabric/                         # fabric-gateway SDK client
│       │   └── client.go                   # SubmitProposal, VerifyProofToken
│       └── ai/
│           └── validator.go                # AI Validator client (LLM sidecar)
│
├── bevel/
│   └── network.yaml                # Deploy automatizado da rede Fabric no K8s
│                                   # 4 orgs, 4 orderers SmartBFT, CouchDB
│
└── docs/
    └── example-ccr-networkpolicy.yaml  # Exemplo comentado de uso real
```

---

## Dependências

### Runtime (o que precisa existir no cluster)

| Dependência | Versão mínima | Finalidade |
|-------------|---------------|------------|
| Kubernetes | 1.28+ | Plataforma base; suporte a ValidatingAdmissionPolicy e SSA |
| Hyperledger Fabric | **3.1.4+** | SmartBFT (BFT consensus) — não usar versões < 3.0 (Raft é CFT apenas) |
| bevel-operator-fabric | 1.9.0+ | Operator Kubernetes para gerenciar peers/orderers/CAs como CRDs |
| Vault (HashiCorp) | 1.14+ | Gerenciamento de chaves criptográficas dos peers e orderers |
| CouchDB | 3.3+ | StateDB dos peers para rich queries no histórico |
| Istio | 1.20+ | Proxy requerido pelo bevel-operator-fabric para mTLS entre peers |
| cert-manager | 1.13+ | TLS automático para o ValidatingAdmissionWebhook |

### Build (desenvolvimento)

| Dependência | Versão | Finalidade |
|-------------|--------|------------|
| Go | 1.22+ | Linguagem do operator, webhook e chaincode |
| Kubebuilder | 3.x | Scaffolding do operator, geração de CRDs e webhook manifests |
| controller-gen | 0.14+ | Geração dos YAML de CRD a partir dos markers no código Go |
| fabric-gateway | 1.4+ | SDK Go para submeter transações e fazer queries no Fabric |
| fabric-contract-api-go | 2.0+ | API para escrever chaincodes em Go |
| Ansible | 2.15+ | Automação do deploy via Bevel |
| Helm | 3.x | Charts do bevel-operator-fabric |
| Docker | 24+ | Build das imagens do chaincode e do operator |

### AI Validator (sidecar)

Compatível com qualquer API OpenAI-compatible. Opções recomendadas:

| Opção | Modelo sugerido | Quando usar |
|-------|----------------|-------------|
| Ollama (local) | `qwen2.5-coder:14b` | Air-gapped / compliance / sem dados saindo |
| OpenAI | `gpt-4o` | Qualidade máxima, custo por token |
| Azure OpenAI | `gpt-4o` | Enterprise, dados dentro da região |
| Anthropic Claude | `claude-sonnet-4-6` | Raciocínio técnico de alta qualidade |

---

## Instalação

### Pré-requisitos

```bash
# Verificar versões
kubectl version --client
go version              # >= 1.22
helm version            # >= 3.x
ansible --version       # >= 2.15

# Instalar kubebuilder
curl -L -o kubebuilder "https://go.kubebuilder.io/dl/latest/$(go env GOOS)/$(go env GOARCH)"
chmod +x kubebuilder && sudo mv kubebuilder /usr/local/bin/

# Instalar cert-manager no cluster (requerido pelo webhook)
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=120s
```

---

### Passo 1 — Deploy da rede Hyperledger Fabric (via Bevel)

```bash
# 1.1 Instalar o bevel-operator-fabric no cluster
kubectl apply -f https://github.com/hyperledger-bevel/bevel-operator-fabric/releases/latest/download/install.yaml
kubectl wait --for=condition=Available deployment --all -n bevel-operator-fabric-system --timeout=180s

# 1.2 Verificar CRDs do Fabric operator disponíveis
kubectl get crd | grep hlf

# 1.3 Configurar credenciais no network.yaml
# Edite bevel/network.yaml com suas credenciais de Vault e contextos K8s

# 1.4 Executar o playbook Bevel para criar a rede KubeGov
# Isso cria: 4 CAs, 4 orgs de peers, 4 orderers SmartBFT, o channel kubegov
ansible-playbook run.yaml -e "@bevel/network.yaml" -v

# 1.5 Verificar que todos os pods estão Running
kubectl get pods -n kubechain-fabric-infra
kubectl get pods -n kubechain-fabric-sec
kubectl get pods -n kubechain-fabric-sre
kubectl get pods -n kubechain-fabric-compliance
kubectl get pods -n kubechain-fabric-orderer

# 1.6 Verificar o channel kubegov criado
# (via CLI do peer, dentro do pod peer0-infra)
kubectl exec -it -n kubechain-fabric-infra deploy/peer0-infra -- \
  peer channel list
# Esperado: kubegov
```

---

### Passo 2 — Deploy do chaincode KubeGov

```bash
# 2.1 Build do chaincode
cd chaincode/
go mod tidy
go build ./...

# 2.2 Empacotar
peer lifecycle chaincode package kubegov.tar.gz \
  --path . \
  --lang golang \
  --label kubegov_1.0

# 2.3 Instalar em todos os peers (repetir para cada org)
for ORG in infra sec sre compliance; do
  kubectl exec -it -n kubechain-fabric-${ORG} deploy/peer0-${ORG} -- \
    peer lifecycle chaincode install kubegov.tar.gz
done

# 2.4 Aprovar em cada org (requer quorum para a endorse policy)
# A endorse policy: AND(infra, sec, OR(sre, compliance))
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

# 2.5 Commit do chaincode no channel
peer lifecycle chaincode commit \
  --channelID kubegov \
  --name kubegov \
  --version 1.0 \
  --sequence 1 \
  --signature-policy "AND('org-infra.member','org-sec.member',OR('org-sre.member','org-compliance.member'))" \
  --peerAddresses peer0-infra.kubechain-fabric-infra:7051 \
  --peerAddresses peer0-sec.kubechain-fabric-sec:7051 \
  --peerAddresses peer0-sre.kubechain-fabric-sre:7051

# 2.6 Verificar chaincode ativo
peer lifecycle chaincode querycommitted --channelID kubegov
```

---

### Passo 3 — Build e deploy do KubeChain Operator

```bash
# 3.1 Inicializar o projeto kubebuilder (gera main.go, Makefile, Dockerfile)
cd operator/
kubebuilder init --domain kubechain.io --repo github.com/sua-org/kubechain
kubebuilder create api --group kubechain --version v1 --kind ConfigChangeRequest

# 3.2 Copiar os arquivos deste repo nos locations corretos gerados pelo kubebuilder
# api/v1/configchangerequest_types.go → já existe no repo
# controllers/configchangerequest_controller.go → já existe no repo
# webhook/validating_webhook.go → já existe no repo

# 3.3 Instalar dependências Go
go mod tidy
# Principais:
# - sigs.k8s.io/controller-runtime
# - k8s.io/apimachinery
# - github.com/hyperledger/fabric-gateway/pkg/client
# - github.com/hyperledger/fabric-contract-api-go

# 3.4 Gerar CRDs e manifests do webhook
make manifests
make generate

# 3.5 Instalar CRDs no cluster
make install

# 3.6 Build e push da imagem Docker
make docker-build docker-push IMG=sua-registry/kubechain-operator:v1.0.0

# 3.7 Deploy do operator no cluster
make deploy IMG=sua-registry/kubechain-operator:v1.0.0

# 3.8 Verificar operator rodando
kubectl get pods -n kubechain-system
# NAME                                    READY   STATUS    RESTARTS
# kubechain-operator-7d9b4c8f6d-xk2p9   2/2     Running   0
# (2/2 = operator + AI validator sidecar)
```

---

### Passo 4 — Configurar o AI Validator

```bash
# Opção A: Ollama local (air-gapped)
# Adicionar ao deployment do operator um sidecar com Ollama

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

# Opção B: Claude API
kubectl create secret generic kubechain-ai-secret \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  -n kubechain-system
```

---

### Passo 5 — Configurar o ValidatingAdmissionWebhook

```bash
# O cert-manager cria o TLS automaticamente para o webhook
# O kubebuilder gerou o manifesto base em config/webhook/

# Aplicar o ValidatingWebhookConfiguration
kubectl apply -f config/webhook/manifests.yaml

# Verificar webhook registrado
kubectl get validatingwebhookconfigurations
# NAME                          WEBHOOKS   AGE
# kubechain-validating-webhook  1          30s

# Testar que o webhook está ativo (deve bloquear)
kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: test-sem-prova
  namespace: default
spec:
  podSelector: {}
EOF
# Esperado: Error from server: "KubeChain: mudança rejeitada — annotation
#           kubechain.io/proof-token ausente. Crie um ConfigChangeRequest..."
```

---

### Passo 6 — Primeira mudança real

```bash
# Criar um ConfigChangeRequest
kubectl apply -f docs/example-ccr-networkpolicy.yaml

# Acompanhar o pipeline em tempo real
kubectl get ccr -n kubechain-system -w

# NAME                                PHASE          AISCORE   PROOFTOKEN
# allow-payments-to-database-netpol   Pending
# allow-payments-to-database-netpol   AIValidating
# allow-payments-to-database-netpol   FabricPending
# allow-payments-to-database-netpol   Approved       0.12      kt_7f3a9b2c...
# allow-payments-to-database-netpol   Applied        0.12      kt_7f3a9b2c...

# Ver detalhes completos
kubectl describe ccr allow-payments-to-database-netpol -n kubechain-system

# Verificar que a NetworkPolicy foi aplicada com o proof token
kubectl get networkpolicy allow-payments-to-database -n database -o yaml | grep kubechain

# kubechain.io/proof-token: "kt_7f3a9b2c1d4e5f6a..."
# kubechain.io/fabric-txid: "a3f8b2c1d4e5..."
# kubechain.io/ccr-name: "allow-payments-to-database-netpol"
```

---

## Consultar o histórico de auditoria

```bash
# Via kubectl — status do CCR
kubectl get ccr -A --sort-by=.metadata.creationTimestamp

# Via chaincode query — histórico completo no ledger Fabric
kubectl exec -it -n kubechain-fabric-infra deploy/peer0-infra -- \
  peer chaincode query \
  --channelID kubegov \
  --name kubegov \
  --ctor '{"function":"GetProposalHistory","Args":["NetworkPolicy","allow-payments-to-database","database"]}'

# Via Grafana (se configurado com CouchDB como datasource)
# Dashboard: KubeChain Audit Trail
# Mostra: propostas por período, taxa de aprovação/rejeição,
#         AI score médio, distribuição por resource kind
```

---

## Configuração da endorse policy

A endorse policy define quem precisa concordar com uma mudança. Edite em `bevel/network.yaml` e reinstale o chaincode.

```
# Política padrão (recomendada para produção):
# Infra + Sec obrigatórios + pelo menos 1 de SRE ou Compliance
AND('org-infra.member', 'org-sec.member', OR('org-sre.member', 'org-compliance.member'))

# Política mais restritiva (mudanças em produção crítica):
# Todos os 4 times precisam concordar
AND('org-infra.member', 'org-sec.member', 'org-sre.member', 'org-compliance.member')

# Política mais permissiva (ambiente de staging):
# Qualquer 2 de 4
OutOf(2, 'org-infra.member', 'org-sec.member', 'org-sre.member', 'org-compliance.member')
```

---

## Tolerância a falhas (SmartBFT)

Com **n orderers**, o SmartBFT tolera **f = ⌊(n-1)/3⌋** orderers maliciosos simultaneamente:

| Orderers | Falhas toleradas | Caso de uso |
|----------|-----------------|-------------|
| 4 | 1 | Ambiente menor, desenvolvimento |
| 7 | 2 | **Produção — recomendado** |
| 10 | 3 | Alta criticidade, múltiplas regiões |

Diferente do Raft (que é apenas CFT — Crash Fault Tolerant), o SmartBFT tolera orderers **ativamente maliciosos**, não apenas crashados. Um orderer comprometido que tenta emitir blocos falsos é detectado e ignorado pelo protocolo.

---

## Variáveis de ambiente do operator

| Variável | Padrão | Descrição |
|----------|--------|-----------|
| `FABRIC_GATEWAY_ENDPOINT` | — | Endpoint gRPC do peer gateway (ex: `peer0-infra:7051`) |
| `FABRIC_CHANNEL_NAME` | `kubegov` | Nome do channel Fabric |
| `FABRIC_CHAINCODE_NAME` | `kubegov` | Nome do chaincode |
| `FABRIC_MSP_ID` | `org-infra` | MSP ID da org que o operator representa |
| `FABRIC_TLS_CERT_PATH` | — | Path para o certificado TLS do peer (montado via Vault) |
| `AI_VALIDATOR_URL` | `http://localhost:11434` | URL do AI Validator sidecar |
| `AI_VALIDATOR_MODEL` | `qwen2.5-coder:14b` | Modelo a usar |
| `AI_RISK_THRESHOLD` | `0.85` | Score acima do qual a proposta é rejeitada |
| `WEBHOOK_FAIL_CLOSED` | `true` | Se `false`, libera quando Fabric está indisponível (não recomendado) |

---

## O que NÃO é coberto por este MVP

Este projeto é uma referência arquitetural e MVP. Para produção, adicionar:

- [ ] `pkg/fabric/client.go` — implementação completa do fabric-gateway SDK com mTLS, reconnect e retry
- [ ] `main.go` — gerado pelo kubebuilder, registra controller + webhook no manager
- [ ] Testes de integração com rede Fabric em Kind/Minikube
- [ ] Política de revogação de ProofTokens (para rollback de mudanças)
- [ ] Multi-cluster: um KubeChain para vários clusters K8s compartilhando o mesmo ledger
- [ ] Dashboard Grafana com CouchDB como datasource
- [ ] Zero-Knowledge proof do AI score (provar que score < threshold sem revelar o modelo)
- [ ] Integração com Sigstore/Cosign para assinar os manifests antes do submit ao Fabric

---

## Referências

- [Hyperledger Fabric 3.x + SmartBFT](https://hyperledger-fabric.readthedocs.io/en/release-3.0/)
- [Hyperledger Bevel](https://hyperledger-bevel.readthedocs.io/)
- [bevel-operator-fabric](https://github.com/hyperledger-bevel/bevel-operator-fabric)
- [Kubebuilder Book](https://book.kubebuilder.io/)
- [fabric-gateway Go SDK](https://pkg.go.dev/github.com/hyperledger/fabric-gateway/pkg/client)
- [Chainlink: Cryptographic Truth](https://blog.chain.link/what-is-cryptographic-truth/)
- [ValidatingAdmissionWebhook Kubernetes](https://kubernetes.io/docs/reference/access-authn-authz/admission-controllers/)

---

## Licença

Apache 2.0 — ver `LICENSE`.

*Desenvolvido como referência arquitetural para governança de clusters Kubernetes enterprise.*  
*Não auditado para produção — use com revisão de segurança adicional.*
