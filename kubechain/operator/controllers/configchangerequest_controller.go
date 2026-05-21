// controllers/configchangerequest_controller.go
package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kubechainv1 "github.com/qintess/kubechain/api/v1"
	"github.com/qintess/kubechain/pkg/ai"
	"github.com/qintess/kubechain/pkg/fabric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ConfigChangeRequestReconciler é o controller principal do KubeChain.
// Ele observa ConfigChangeRequests e orquestra o pipeline:
//   Pending → AIValidating → FabricPending → Approved → Applied
//
// Em cada fase, atualiza o .status do CCR para rastreabilidade.
type ConfigChangeRequestReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	FabricClient  *fabric.KubeGovClient
	AIClient      *ai.ValidatorClient
}

// +kubebuilder:rbac:groups=kubechain.io,resources=configchangerequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubechain.io,resources=configchangerequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubechain.io,resources=configchangerequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;create;update;patch

func (r *ConfigChangeRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// --- 1. Busca o CCR ---
	ccr := &kubechainv1.ConfigChangeRequest{}
	if err := r.Get(ctx, req.NamespacedName, ccr); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("Reconciling CCR", "name", ccr.Name, "phase", ccr.Status.Phase)

	switch ccr.Status.Phase {
	case "", kubechainv1.PhasePending:
		return r.handlePending(ctx, ccr)

	case kubechainv1.PhaseAIValidating:
		return r.handleAIValidating(ctx, ccr)

	case kubechainv1.PhaseFabricPend:
		return r.handleFabricPending(ctx, ccr)

	case kubechainv1.PhaseApproved:
		return r.handleApproved(ctx, ccr)

	case kubechainv1.PhaseApplied, kubechainv1.PhaseRejected, kubechainv1.PhaseFailed:
		// Estados terminais — nada a fazer
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// handlePending: extrai o diff e dispara a validação por IA.
func (r *ConfigChangeRequestReconciler) handlePending(
	ctx context.Context, ccr *kubechainv1.ConfigChangeRequest,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Phase: Pending → AIValidating", "ccr", ccr.Name)

	// Busca o estado atual do resource no cluster para gerar o diff
	currentState, err := r.fetchCurrentResource(ctx, ccr)
	if err != nil {
		return r.setFailed(ctx, ccr, fmt.Sprintf("erro ao buscar estado atual: %v", err))
	}

	// Serializa o diff para o AI Validator
	diffPayload := &ai.DiffPayload{
		ResourceKind:         ccr.Spec.ResourceKind,
		ResourceName:         ccr.Spec.ResourceName,
		Namespace:            ccr.Spec.Namespace,
		CurrentState:         currentState,
		ProposedManifest:     ccr.Spec.RawManifest.Raw,
		ChangelogDescription: ccr.Spec.ChangelogDescription,
		PRReference:          ccr.Spec.PRReference,
		RequestedBy:          ccr.Spec.RequestedBy,
	}

	// Transição de fase antes de chamar IA (para idempotência em requeue)
	ccr.Status.Phase = kubechainv1.PhaseAIValidating
	if err := r.Status().Update(ctx, ccr); err != nil {
		return ctrl.Result{}, err
	}

	// Chama o AI Validator
	result, err := r.AIClient.Validate(ctx, diffPayload)
	if err != nil {
		return r.setFailed(ctx, ccr, fmt.Sprintf("AI Validator indisponível: %v", err))
	}

	log.Info("AI Validation result", "score", result.RiskScore, "reasoning", result.Summary)

	// Se risco crítico, rejeita imediatamente
	if result.RiskScore >= 0.85 {
		return r.setRejected(ctx, ccr, result.RiskScore, result.Summary,
			fmt.Sprintf("AI risk score %.2f excede threshold 0.85: %s", result.RiskScore, result.Summary))
	}

	// Avança para submissão no Fabric
	ccr.Status.AIScore = result.RiskScore
	ccr.Status.AIReasoning = result.Summary
	ccr.Status.Phase = kubechainv1.PhaseFabricPend
	if err := r.Status().Update(ctx, ccr); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleFabricPending: submete a transação ao Hyperledger Fabric.
func (r *ConfigChangeRequestReconciler) handleFabricPending(
	ctx context.Context, ccr *kubechainv1.ConfigChangeRequest,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Phase: FabricPending → submetendo transação", "ccr", ccr.Name)

	proposal := &fabric.ChangeProposal{
		CCRID:                ccr.Name,
		CCRNamespace:         ccr.Namespace,
		ResourceKind:         ccr.Spec.ResourceKind,
		ResourceName:         ccr.Spec.ResourceName,
		K8sNamespace:         ccr.Spec.Namespace,
		ManifestHash:         hashManifest(ccr.Spec.RawManifest.Raw),
		AIScore:              ccr.Status.AIScore,
		AIReasoning:          ccr.Status.AIReasoning,
		ChangelogDescription: ccr.Spec.ChangelogDescription,
		PRReference:          ccr.Spec.PRReference,
		RequestedBy:          ccr.Spec.RequestedBy,
		Urgency:              ccr.Spec.Urgency,
		ProposedAt:           time.Now().UTC().Format(time.RFC3339),
	}

	// SubmitProposal faz:
	// 1. Invoca chaincode ProposeChange nos peers (endorse)
	// 2. Submete ao orderer (SmartBFT consensus)
	// 3. Aguarda commit no ledger
	// 4. Retorna o ProofToken (block hash + tx ID)
	proof, err := r.FabricClient.SubmitProposal(ctx, proposal)
	if err != nil {
		log.Error(err, "Falha ao submeter ao Fabric, requeue em 30s")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	log.Info("Transação confirmada no Fabric",
		"txId", proof.TxID,
		"block", proof.BlockNumber,
		"endorsers", proof.Endorsers,
	)

	ccr.Status.FabricTxID = proof.TxID
	ccr.Status.ProofToken = proof.ProofToken
	ccr.Status.BlockNumber = proof.BlockNumber
	ccr.Status.Endorsers = proof.Endorsers
	ccr.Status.Phase = kubechainv1.PhaseApproved

	now := metav1.Now()
	ccr.Status.Conditions = append(ccr.Status.Conditions, metav1.Condition{
		Type:               "FabricConsensusReached",
		Status:             metav1.ConditionTrue,
		Reason:             "BlockCommitted",
		Message:            fmt.Sprintf("bloco %d, tx %s", proof.BlockNumber, proof.TxID),
		LastTransitionTime: now,
	})

	if err := r.Status().Update(ctx, ccr); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleApproved: aplica o manifest ao cluster com o ProofToken como annotation.
// O ValidatingWebhook vai verificar esse token ao interceptar a chamada.
func (r *ConfigChangeRequestReconciler) handleApproved(
	ctx context.Context, ccr *kubechainv1.ConfigChangeRequest,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Phase: Approved → aplicando manifest", "ccr", ccr.Name, "proofToken", ccr.Status.ProofToken)

	// Deserializa o manifest proposto
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(ccr.Spec.RawManifest.Raw, obj); err != nil {
		return r.setFailed(ctx, ccr, fmt.Sprintf("manifest inválido: %v", err))
	}

	// Injeta as annotations de prova ANTES de aplicar.
	// O ValidatingWebhook exige essas annotations para permitir a operação.
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[kubechainv1.AnnotationProofToken] = ccr.Status.ProofToken
	annotations[kubechainv1.AnnotationFabricTxID] = ccr.Status.FabricTxID
	annotations["kubechain.io/ccr-name"] = ccr.Name
	annotations["kubechain.io/ccr-namespace"] = ccr.Namespace
	obj.SetAnnotations(annotations)

	// Apply via Server-Side Apply (SSA) para idempotência
	data, err := json.Marshal(obj)
	if err != nil {
		return r.setFailed(ctx, ccr, fmt.Sprintf("erro serializando manifest: %v", err))
	}

	if err := r.Patch(ctx, obj, client.RawPatch(types.ApplyPatchType, data),
		client.ForceOwnership, client.FieldOwner("kubechain-operator")); err != nil {
		return r.setFailed(ctx, ccr, fmt.Sprintf("erro aplicando manifest: %v", err))
	}

	now := metav1.Now()
	ccr.Status.Phase = kubechainv1.PhaseApplied
	ccr.Status.AppliedAt = &now
	ccr.Status.Conditions = append(ccr.Status.Conditions, metav1.Condition{
		Type:               "Applied",
		Status:             metav1.ConditionTrue,
		Reason:             "ManifestApplied",
		Message:            fmt.Sprintf("aplicado ao cluster em %s", now.Format(time.RFC3339)),
		LastTransitionTime: now,
	})

	log.Info("Manifest aplicado com sucesso", "ccr", ccr.Name,
		"kind", ccr.Spec.ResourceKind, "name", ccr.Spec.ResourceName)

	return ctrl.Result{}, r.Status().Update(ctx, ccr)
}

// handleAIValidating: caso o processo seja reiniciado durante AIValidating,
// retorna para Pending para re-executar a validação.
func (r *ConfigChangeRequestReconciler) handleAIValidating(
	ctx context.Context, ccr *kubechainv1.ConfigChangeRequest,
) (ctrl.Result, error) {
	ccr.Status.Phase = kubechainv1.PhasePending
	return ctrl.Result{Requeue: true}, r.Status().Update(ctx, ccr)
}

// --- Helpers ---

func (r *ConfigChangeRequestReconciler) setFailed(
	ctx context.Context, ccr *kubechainv1.ConfigChangeRequest, reason string,
) (ctrl.Result, error) {
	ccr.Status.Phase = kubechainv1.PhaseFailed
	ccr.Status.RejectionReason = reason
	return ctrl.Result{}, r.Status().Update(ctx, ccr)
}

func (r *ConfigChangeRequestReconciler) setRejected(
	ctx context.Context, ccr *kubechainv1.ConfigChangeRequest,
	score float64, reasoning, reason string,
) (ctrl.Result, error) {
	ccr.Status.Phase = kubechainv1.PhaseRejected
	ccr.Status.AIScore = score
	ccr.Status.AIReasoning = reasoning
	ccr.Status.RejectionReason = reason
	return ctrl.Result{}, r.Status().Update(ctx, ccr)
}

func (r *ConfigChangeRequestReconciler) fetchCurrentResource(
	ctx context.Context, ccr *kubechainv1.ConfigChangeRequest,
) ([]byte, error) {
	obj := &unstructured.Unstructured{}
	obj.SetKind(ccr.Spec.ResourceKind)
	obj.SetName(ccr.Spec.ResourceName)
	obj.SetNamespace(ccr.Spec.Namespace)

	err := r.Get(ctx, client.ObjectKey{
		Name:      ccr.Spec.ResourceName,
		Namespace: ccr.Spec.Namespace,
	}, obj)

	if errors.IsNotFound(err) {
		return []byte("{}"), nil // resource novo, sem estado atual
	}
	if err != nil {
		return nil, err
	}

	return json.Marshal(obj.Object)
}

func hashManifest(data []byte) string {
	// SHA-256 do manifest para gravar no ledger Fabric
	import_hash := "crypto/sha256" // simplificado — import real no arquivo final
	_ = import_hash
	return fmt.Sprintf("%x", data) // placeholder — use sha256.Sum256(data)
}

func (r *ConfigChangeRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubechainv1.ConfigChangeRequest{}).
		Complete(r)
}
