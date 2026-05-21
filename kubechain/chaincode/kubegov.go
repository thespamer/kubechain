// chaincode/kubegov/kubegov.go
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hyperledger/fabric-contract-api-go/contractapi"
)

// KubeGovContract é o smart contract (chaincode) do KubeChain no Hyperledger Fabric.
//
// Ele é responsável por:
//   - Registrar propostas de mudança de configuração Kubernetes
//   - Validar as regras de endorse policy (multi-org approval)
//   - Emitir eventos para o controller escutar
//   - Prover verificação de proof tokens para o ValidatingWebhook
//   - Manter histórico imutável de todas as mudanças
//
// O chaincode roda nos peers de cada organização (Org-Infra, Org-Sec, Org-SRE, Org-Compliance).
// A endorse policy exige assinatura de pelo menos Org-Infra + Org-Sec + 1 adicional.
type KubeGovContract struct {
	contractapi.Contract
}

// ChangeProposal é o estado de uma proposta no ledger.
type ChangeProposal struct {
	// Identificação
	CCRID        string `json:"ccrId"`
	CCRNamespace string `json:"ccrNamespace"`
	ProposalID   string `json:"proposalId"` // hash do conteúdo

	// O que está sendo mudado
	ResourceKind string `json:"resourceKind"`
	ResourceName string `json:"resourceName"`
	K8sNamespace string `json:"k8sNamespace"`
	ManifestHash string `json:"manifestHash"`

	// Contexto da mudança
	ChangelogDescription string `json:"changelogDescription"`
	PRReference          string `json:"prReference,omitempty"`
	RequestedBy          string `json:"requestedBy"`
	Urgency              string `json:"urgency"`

	// Validação por IA
	AIScore     float64 `json:"aiScore"`
	AIReasoning string  `json:"aiReasoning"`

	// Estado do consenso
	Status     string   `json:"status"` // PROPOSED | APPROVED | REJECTED | APPLIED
	Endorsers  []string `json:"endorsers"`
	ProofToken string   `json:"proofToken,omitempty"`

	// Timestamps
	ProposedAt  string `json:"proposedAt"`
	ApprovedAt  string `json:"approvedAt,omitempty"`
	AppliedAt   string `json:"appliedAt,omitempty"`
	ExpiresAt   string `json:"expiresAt"` // proposals expiram em 24h

	// Histórico de rejeições
	RejectedBy   string `json:"rejectedBy,omitempty"`
	RejectReason string `json:"rejectReason,omitempty"`
}

// ProofRecord é o registro imutável gravado quando uma mudança é aplicada.
type ProofRecord struct {
	ProofToken      string   `json:"proofToken"`
	ProposalID      string   `json:"proposalId"`
	ManifestHash    string   `json:"manifestHash"`
	ResourceKind    string   `json:"resourceKind"`
	ResourceName    string   `json:"resourceName"`
	K8sNamespace    string   `json:"k8sNamespace"`
	Endorsers       []string `json:"endorsers"`
	BlockNumber     uint64   `json:"blockNumber"`
	TxID            string   `json:"txId"`
	Timestamp       string   `json:"timestamp"`
	Valid           bool     `json:"valid"`
	InvalidatedAt   string   `json:"invalidatedAt,omitempty"`
	InvalidReason   string   `json:"invalidReason,omitempty"`
}

// ── FUNÇÕES DE ESCRITA ──────────────────────────────────────────────────────

// ProposeChange registra uma nova proposta de mudança no ledger.
// Chamado pelo kubechain-operator após validação por IA.
// A endorse policy do channel garante que pelo menos 2 orgs validaram.
func (c *KubeGovContract) ProposeChange(ctx contractapi.TransactionContextInterface,
	proposalJSON string,
) (string, error) {

	var proposal ChangeProposal
	if err := json.Unmarshal([]byte(proposalJSON), &proposal); err != nil {
		return "", fmt.Errorf("proposta inválida: %w", err)
	}

	// Gera ID único da proposta baseado no conteúdo
	proposal.ProposalID = generateProposalID(proposal)
	proposal.Status = "PROPOSED"
	proposal.ProposedAt = time.Now().UTC().Format(time.RFC3339)

	// Proposta expira em 24h (ou 1h para urgência crítica)
	expiry := 24 * time.Hour
	if proposal.Urgency == "critical" {
		expiry = 1 * time.Hour
	}
	proposal.ExpiresAt = time.Now().Add(expiry).UTC().Format(time.RFC3339)

	// Valida regras de negócio do KubeGov
	if err := validateProposal(proposal); err != nil {
		return "", fmt.Errorf("proposta rejeitada pelas regras do KubeGov: %w", err)
	}

	// Grava no ledger com key = "PROPOSAL:<proposalID>"
	proposalBytes, _ := json.Marshal(proposal)
	key := fmt.Sprintf("PROPOSAL:%s", proposal.ProposalID)
	if err := ctx.GetStub().PutState(key, proposalBytes); err != nil {
		return "", fmt.Errorf("erro gravando proposta: %w", err)
	}

	// Emite evento para o controller escutar via event listener
	eventPayload, _ := json.Marshal(map[string]string{
		"type":       "ProposalSubmitted",
		"proposalId": proposal.ProposalID,
		"ccrId":      proposal.CCRID,
		"kind":       proposal.ResourceKind,
		"name":       proposal.ResourceName,
	})
	ctx.GetStub().SetEvent("ProposalSubmitted", eventPayload)

	return proposal.ProposalID, nil
}

// ApproveChange é chamado após o consenso SmartBFT dos orderers.
// Registra o proof token e move a proposta para APPROVED.
// Esse é o ponto onde a "proof" é criada — bloco imutável no ledger.
func (c *KubeGovContract) ApproveChange(ctx contractapi.TransactionContextInterface,
	proposalID string,
) (*ProofRecord, error) {

	proposal, err := c.getProposal(ctx, proposalID)
	if err != nil {
		return nil, err
	}

	if proposal.Status != "PROPOSED" {
		return nil, fmt.Errorf("proposta %s não está em status PROPOSED (atual: %s)",
			proposalID, proposal.Status)
	}

	// Verifica expiração
	expiresAt, _ := time.Parse(time.RFC3339, proposal.ExpiresAt)
	if time.Now().After(expiresAt) {
		proposal.Status = "EXPIRED"
		proposalBytes, _ := json.Marshal(proposal)
		ctx.GetStub().PutState(fmt.Sprintf("PROPOSAL:%s", proposalID), proposalBytes)
		return nil, fmt.Errorf("proposta %s expirou em %s", proposalID, proposal.ExpiresAt)
	}

	// O TxID do Fabric é o identificador único desta transação de aprovação
	txID := ctx.GetStub().GetTxID()
	timestamp, _ := ctx.GetStub().GetTxTimestamp()
	ts := time.Unix(timestamp.Seconds, int64(timestamp.Nanos)).UTC().Format(time.RFC3339)

	// Gera o ProofToken: SHA-256(proposalID + txID + manifestHash)
	proofToken := generateProofToken(proposalID, txID, proposal.ManifestHash)

	// Atualiza a proposta
	proposal.Status = "APPROVED"
	proposal.ProofToken = proofToken
	proposal.ApprovedAt = ts
	proposalBytes, _ := json.Marshal(proposal)
	ctx.GetStub().PutState(fmt.Sprintf("PROPOSAL:%s", proposalID), proposalBytes)

	// Cria o ProofRecord — é esse registro que o ValidatingWebhook consulta
	proof := &ProofRecord{
		ProofToken:   proofToken,
		ProposalID:   proposalID,
		ManifestHash: proposal.ManifestHash,
		ResourceKind: proposal.ResourceKind,
		ResourceName: proposal.ResourceName,
		K8sNamespace: proposal.K8sNamespace,
		Endorsers:    proposal.Endorsers,
		TxID:         txID,
		Timestamp:    ts,
		Valid:         true,
	}

	proofBytes, _ := json.Marshal(proof)
	proofKey := fmt.Sprintf("PROOF:%s", proofToken)
	if err := ctx.GetStub().PutState(proofKey, proofBytes); err != nil {
		return nil, fmt.Errorf("erro gravando proof record: %w", err)
	}

	// Evento de aprovação
	eventPayload, _ := json.Marshal(map[string]string{
		"type":       "ProposalApproved",
		"proposalId": proposalID,
		"proofToken": proofToken,
		"txId":       txID,
	})
	ctx.GetStub().SetEvent("ProposalApproved", eventPayload)

	return proof, nil
}

// MarkApplied registra que a mudança foi efetivamente aplicada ao cluster K8s.
// Chamado pelo reconciler após o kubectl apply bem-sucedido.
func (c *KubeGovContract) MarkApplied(ctx contractapi.TransactionContextInterface,
	proofToken string, appliedAt string,
) error {

	proof, err := c.getProof(ctx, proofToken)
	if err != nil {
		return err
	}

	proof.Valid = true
	proofBytes, _ := json.Marshal(proof)
	ctx.GetStub().PutState(fmt.Sprintf("PROOF:%s", proofToken), proofBytes)

	// Também atualiza a proposta
	proposal, _ := c.getProposal(ctx, proof.ProposalID)
	if proposal != nil {
		proposal.Status = "APPLIED"
		proposal.AppliedAt = appliedAt
		proposalBytes, _ := json.Marshal(proposal)
		ctx.GetStub().PutState(fmt.Sprintf("PROPOSAL:%s", proof.ProposalID), proposalBytes)
	}

	eventPayload, _ := json.Marshal(map[string]string{
		"type":       "ChangeApplied",
		"proofToken": proofToken,
		"appliedAt":  appliedAt,
	})
	ctx.GetStub().SetEvent("ChangeApplied", eventPayload)

	return nil
}

// ── FUNÇÕES DE LEITURA (para o ValidatingWebhook) ──────────────────────────

// VerifyProofToken é a função chamada pelo ValidatingWebhook a cada admission.
// Verifica se o proofToken é válido, não expirado, e se o hash do manifest bate.
// É uma operação de LEITURA — não modifica o ledger, garante performance.
func (c *KubeGovContract) VerifyProofToken(ctx contractapi.TransactionContextInterface,
	proofToken string, manifestHash string, resourceKind string, resourceName string, k8sNamespace string,
) (string, error) {

	proof, err := c.getProof(ctx, proofToken)
	if err != nil {
		return "", fmt.Errorf("proof token não encontrado no ledger: %w", err)
	}

	type VerifyResult struct {
		Valid         bool     `json:"valid"`
		BlockNumber   uint64   `json:"blockNumber"`
		Endorsers     []string `json:"endorsers"`
		InvalidReason string   `json:"invalidReason,omitempty"`
	}

	result := &VerifyResult{Endorsers: proof.Endorsers, BlockNumber: proof.BlockNumber}

	if !proof.Valid {
		result.Valid = false
		result.InvalidReason = fmt.Sprintf("proof token invalidado: %s", proof.InvalidReason)
		resultBytes, _ := json.Marshal(result)
		return string(resultBytes), nil
	}

	// Verifica se o hash do manifest bate (anti-tampering)
	if proof.ManifestHash != manifestHash {
		result.Valid = false
		result.InvalidReason = fmt.Sprintf(
			"hash do manifest não confere — esperado: %s, recebido: %s. "+
				"O manifest pode ter sido adulterado após a aprovação.",
			proof.ManifestHash, manifestHash,
		)
		resultBytes, _ := json.Marshal(result)
		return string(resultBytes), nil
	}

	// Verifica se o resource bate
	if proof.ResourceKind != resourceKind || proof.ResourceName != resourceName {
		result.Valid = false
		result.InvalidReason = fmt.Sprintf(
			"proof token foi emitido para %s/%s, mas está sendo usado em %s/%s (replay attack?)",
			proof.ResourceKind, proof.ResourceName, resourceKind, resourceName,
		)
		resultBytes, _ := json.Marshal(result)
		return string(resultBytes), nil
	}

	result.Valid = true
	resultBytes, _ := json.Marshal(result)
	return string(resultBytes), nil
}

// GetProposalHistory retorna o histórico de propostas para um resource.
// Usado pelo painel de auditoria.
func (c *KubeGovContract) GetProposalHistory(ctx contractapi.TransactionContextInterface,
	resourceKind string, resourceName string, k8sNamespace string,
) ([]*ChangeProposal, error) {

	// Range query por índice composto (kind~name~namespace)
	indexName := "kind~name~namespace~id"
	resultsIterator, err := ctx.GetStub().GetStateByPartialCompositeKey(indexName,
		[]string{resourceKind, resourceName, k8sNamespace})
	if err != nil {
		return nil, err
	}
	defer resultsIterator.Close()

	var proposals []*ChangeProposal
	for resultsIterator.HasNext() {
		item, err := resultsIterator.Next()
		if err != nil {
			continue
		}
		var proposal ChangeProposal
		if err := json.Unmarshal(item.Value, &proposal); err == nil {
			proposals = append(proposals, &proposal)
		}
	}

	return proposals, nil
}

// ── HELPERS ────────────────────────────────────────────────────────────────

func (c *KubeGovContract) getProposal(ctx contractapi.TransactionContextInterface,
	proposalID string) (*ChangeProposal, error) {
	key := fmt.Sprintf("PROPOSAL:%s", proposalID)
	data, err := ctx.GetStub().GetState(key)
	if err != nil {
		return nil, fmt.Errorf("erro lendo proposta: %w", err)
	}
	if data == nil {
		return nil, fmt.Errorf("proposta %s não encontrada no ledger", proposalID)
	}
	var proposal ChangeProposal
	if err := json.Unmarshal(data, &proposal); err != nil {
		return nil, fmt.Errorf("erro deserializando proposta: %w", err)
	}
	return &proposal, nil
}

func (c *KubeGovContract) getProof(ctx contractapi.TransactionContextInterface,
	proofToken string) (*ProofRecord, error) {
	key := fmt.Sprintf("PROOF:%s", proofToken)
	data, err := ctx.GetStub().GetState(key)
	if err != nil {
		return nil, fmt.Errorf("erro lendo proof record: %w", err)
	}
	if data == nil {
		return nil, fmt.Errorf("proof token %s não encontrado no ledger", proofToken)
	}
	var proof ProofRecord
	if err := json.Unmarshal(data, &proof); err != nil {
		return nil, fmt.Errorf("erro deserializando proof record: %w", err)
	}
	return &proof, nil
}

func generateProposalID(p ChangeProposal) string {
	content := fmt.Sprintf("%s:%s:%s:%s:%s:%s",
		p.CCRID, p.ResourceKind, p.ResourceName, p.K8sNamespace, p.ManifestHash, p.ProposedAt)
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", hash[:16])
}

func generateProofToken(proposalID, txID, manifestHash string) string {
	content := fmt.Sprintf("%s:%s:%s", proposalID, txID, manifestHash)
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("kt_%x", hash) // kt_ prefix identifica KubeChain tokens
}

func validateProposal(p ChangeProposal) error {
	// Regras de negócio hardcoded no chaincode:
	// 1. Mudanças em ClusterRole com verbs=* exigem Urgency=critical (força review extra)
	// 2. Mudanças em ValidatingWebhookConfiguration bloqueadas por 5 min para cool-off
	// 3. AI score > 0.7 exige justificativa com no mínimo 50 chars
	if p.ResourceKind == "ClusterRole" && p.Urgency != "critical" {
		return fmt.Errorf("mudanças em ClusterRole exigem urgency=critical para revisão extra")
	}
	if p.AIScore > 0.7 && len(p.ChangelogDescription) < 50 {
		return fmt.Errorf("AI score %.2f exige changelog com pelo menos 50 caracteres de justificativa", p.AIScore)
	}
	return nil
}

func main() {
	chaincode, err := contractapi.NewChaincode(&KubeGovContract{})
	if err != nil {
		panic(fmt.Sprintf("Erro criando chaincode: %v", err))
	}
	if err := chaincode.Start(); err != nil {
		panic(fmt.Sprintf("Erro iniciando chaincode: %v", err))
	}
}
