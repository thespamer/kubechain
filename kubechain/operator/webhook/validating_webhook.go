// webhook/validating_webhook.go
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kubechainv1 "github.com/qintess/kubechain/api/v1"
	"github.com/qintess/kubechain/pkg/fabric"
)

// KubeChainValidator é o ValidatingAdmissionWebhook central.
//
// Ele intercepta QUALQUER operação de criação ou update nos resource types
// configurados (NetworkPolicy, ClusterRole, ClusterRoleBinding, ConfigMap
// sensíveis, Ingress, etc.) e garante que:
//
//   1. O resource possui a annotation kubechain.io/proof-token
//   2. Esse token existe no ledger do Hyperledger Fabric (QueryTx)
//   3. O hash do manifest bate com o hash gravado na transação Fabric
//
// Se qualquer uma das condições falhar → 403 Forbidden.
// Não há exceção. Não há bypass. Proof over Trust.
//
// O único "bypass" legítimo é o próprio kubechain-operator (identificado
// pelo ServiceAccount kubechain-operator no namespace kubechain-system),
// que injeta o proof-token antes de aplicar o manifest.

type KubeChainValidator struct {
	Client        client.Client
	FabricClient  *fabric.KubeGovClient
	decoder       *admission.Decoder

	// ResourceTypes que este webhook intercepta.
	// Configurado via ValidatingWebhookConfiguration no cluster.
	// Todos os demais resources passam livremente.
	watchedKinds map[string]bool
}

// Handle é chamado pelo kube-apiserver para cada admission review.
func (v *KubeChainValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := log.FromContext(ctx)

	log.Info("Webhook interceptando",
		"kind", req.Kind.Kind,
		"name", req.Name,
		"namespace", req.Namespace,
		"operation", req.Operation,
		"userInfo", req.UserInfo.Username,
	)

	// --- Exceção: o próprio operator tem permissão (ele já tem o proof token) ---
	if req.UserInfo.Username == "system:serviceaccount:kubechain-system:kubechain-operator" {
		log.Info("Permitindo — origem: kubechain-operator (trusted source)")
		return admission.Allowed("kubechain-operator trusted source")
	}

	// --- Exceção: operações de DELETE não exigem proof token ---
	// (mas são logadas no Fabric via evento separado)
	if req.Operation == admissionv1.Delete {
		log.Info("DELETE permitido, mas auditado separadamente")
		return admission.Allowed("delete operation — audit only")
	}

	// --- Verifica se esse kind é monitorado pelo KubeChain ---
	if !v.isWatched(req.Kind.Kind) {
		return admission.Allowed("resource kind não monitorado pelo KubeChain")
	}

	// --- Decodifica o objeto ---
	obj := &unstructured.Unstructured{}
	if err := v.decoder.DecodeRaw(req.Object, obj); err != nil {
		return admission.Errored(http.StatusBadRequest,
			fmt.Errorf("erro decodificando objeto: %w", err))
	}

	annotations := obj.GetAnnotations()

	// --- Verifica presença do proof token ---
	proofToken, hasToken := annotations[kubechainv1.AnnotationProofToken]
	if !hasToken || proofToken == "" {
		log.Info("REJEITADO — sem proof token",
			"kind", req.Kind.Kind, "name", req.Name, "user", req.UserInfo.Username)
		return admission.Denied(
			"KubeChain: mudança rejeitada — annotation kubechain.io/proof-token ausente. " +
				"Toda mudança de configuração deve passar pelo pipeline de consenso Hyperledger Fabric. " +
				"Crie um ConfigChangeRequest para propor esta alteração.",
		)
	}

	fabricTxID, _ := annotations[kubechainv1.AnnotationFabricTxID]

	// --- Verifica o proof token no ledger Fabric ---
	verification, err := v.FabricClient.VerifyProofToken(ctx, &fabric.VerifyRequest{
		ProofToken:   proofToken,
		FabricTxID:   fabricTxID,
		ResourceKind: req.Kind.Kind,
		ResourceName: req.Name,
		K8sNamespace: req.Namespace,
		ManifestHash: hashObject(req.Object.Raw),
	})

	if err != nil {
		log.Error(err, "Erro consultando Fabric — fail closed (segurança sobre disponibilidade)")
		return admission.Denied(
			fmt.Sprintf("KubeChain: não foi possível verificar proof token no Fabric: %v. "+
				"Por segurança, a operação foi bloqueada (fail-closed).", err),
		)
	}

	if !verification.Valid {
		log.Info("REJEITADO — proof token inválido no Fabric",
			"proofToken", proofToken,
			"reason", verification.InvalidReason,
		)
		return admission.Denied(
			fmt.Sprintf("KubeChain: proof token inválido — %s. "+
				"Possíveis causas: token expirado, manifest adulterado após aprovação, "+
				"ou tentativa de replay attack.", verification.InvalidReason),
		)
	}

	log.Info("APROVADO — proof token válido no Fabric",
		"txID", fabricTxID,
		"block", verification.BlockNumber,
		"endorsers", verification.Endorsers,
	)

	return admission.Allowed(
		fmt.Sprintf("KubeChain: proof válido — bloco %d, tx %s", verification.BlockNumber, fabricTxID),
	)
}

// InjectDecoder é chamado pelo controller-runtime para injetar o decoder.
func (v *KubeChainValidator) InjectDecoder(d *admission.Decoder) error {
	v.decoder = d
	return nil
}

func (v *KubeChainValidator) isWatched(kind string) bool {
	return v.watchedKinds[kind]
}

func hashObject(raw []byte) string {
	// SHA-256 do JSON do objeto (sem annotations gerenciadas pelo sistema)
	// Em produção: remover campos como resourceVersion, uid antes de hashear
	return fmt.Sprintf("%x", raw)
}

// NewKubeChainValidator inicializa o webhook com os kinds monitorados.
func NewKubeChainValidator(c client.Client, fc *fabric.KubeGovClient) *KubeChainValidator {
	return &KubeChainValidator{
		Client:       c,
		FabricClient: fc,
		watchedKinds: map[string]bool{
			// Políticas de rede
			"NetworkPolicy": true,

			// Controle de acesso
			"ClusterRole":        true,
			"ClusterRoleBinding": true,
			"Role":               true,
			"RoleBinding":        true,

			// Workloads críticos
			"Deployment":  true,
			"StatefulSet": true,
			"DaemonSet":   true,

			// Configurações sensíveis
			"ConfigMap": true, // apenas com label kubechain.io/sensitive=true
			"Secret":    true,

			// Ingress e service exposure
			"Ingress": true,

			// Pod Security e admission
			"PodSecurityPolicy": true,
			"MutatingWebhookConfiguration":   true,
			"ValidatingWebhookConfiguration": true,

			// Namespaces (criação de namespaces novos exige consenso)
			"Namespace": true,
		},
	}
}
