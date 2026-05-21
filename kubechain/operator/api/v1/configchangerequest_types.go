// api/v1/configchangerequest_types.go
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ConfigChangeRequestSpec define a mudança proposta.
// O campo RawManifest contém o YAML/JSON do resource que será aplicado.
type ConfigChangeRequestSpec struct {
	// ResourceKind é o tipo do recurso Kubernetes a ser modificado.
	// Ex: NetworkPolicy, ClusterRole, Deployment, ConfigMap
	ResourceKind string `json:"resourceKind"`

	// ResourceName é o nome do resource no namespace alvo.
	ResourceName string `json:"resourceName"`

	// Namespace do resource. Vazio para cluster-scoped.
	Namespace string `json:"namespace,omitempty"`

	// RawManifest é o manifest desejado serializado em JSON.
	// O reconciler usa isso para comparar com o estado atual e montar o diff.
	// +kubebuilder:pruning:PreserveUnknownFields
	RawManifest runtime.RawExtension `json:"rawManifest"`

	// ChangelogDescription é a justificativa humana para a mudança.
	// O AI Validator usa esse campo para checar consistência semântica com o diff.
	ChangelogDescription string `json:"changelogDescription"`

	// PRReference é o link/ID do PR no GitHub/GitLab que originou essa mudança.
	PRReference string `json:"prReference,omitempty"`

	// RequestedBy é a identidade (email/SA) de quem está propondo a mudança.
	RequestedBy string `json:"requestedBy"`

	// Urgency indica se a mudança é crítica (impacta política de expiração do voto).
	// +kubebuilder:validation:Enum=low;medium;high;critical
	// +kubebuilder:default=medium
	Urgency string `json:"urgency,omitempty"`
}

// ConfigChangeRequestStatus reflete o estado atual da proposta.
type ConfigChangeRequestStatus struct {
	// Phase indica o estado da proposta no pipeline de consenso.
	// +kubebuilder:validation:Enum=Pending;AIValidating;FabricPending;Endorsed;Approved;Rejected;Applied;Failed
	Phase string `json:"phase,omitempty"`

	// AIScore é o score de risco calculado pelo AI Validator (0.0 = sem risco, 1.0 = crítico).
	AIScore float64 `json:"aiScore,omitempty"`

	// AIReasoning é o resumo da análise do AI Validator.
	AIReasoning string `json:"aiReasoning,omitempty"`

	// FabricTxID é o ID da transação no Hyperledger Fabric após o submit.
	FabricTxID string `json:"fabricTxId,omitempty"`

	// ProofToken é o hash do bloco confirmado no Fabric.
	// Esse token é injetado como annotation no manifest antes de aplicar ao cluster.
	ProofToken string `json:"proofToken,omitempty"`

	// BlockNumber é o número do bloco no ledger onde a transação foi incluída.
	BlockNumber uint64 `json:"blockNumber,omitempty"`

	// Endorsers lista os peers que endossaram a transação.
	Endorsers []string `json:"endorsers,omitempty"`

	// RejectionReason descreve por que foi rejeitado (se Phase == Rejected).
	RejectionReason string `json:"rejectionReason,omitempty"`

	// Conditions segue o padrão Kubernetes de condições.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// AppliedAt registra quando o manifest foi efetivamente aplicado ao cluster.
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ccr
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.resourceKind`
// +kubebuilder:printcolumn:name="Resource",type=string,JSONPath=`.spec.resourceName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="AIScore",type=number,JSONPath=`.status.aiScore`
// +kubebuilder:printcolumn:name="ProofToken",type=string,JSONPath=`.status.proofToken`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ConfigChangeRequest é o recurso central do KubeChain.
// Toda mudança de configuração no cluster DEVE ser submetida como um CCR.
// Sem um CCR aprovado e com ProofToken válido no Fabric, o ValidatingWebhook
// bloqueia a operação diretamente no kube-apiserver.
type ConfigChangeRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConfigChangeRequestSpec   `json:"spec,omitempty"`
	Status ConfigChangeRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ConfigChangeRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConfigChangeRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ConfigChangeRequest{}, &ConfigChangeRequestList{})
}

// Constantes de Phase para uso nos controllers
const (
	PhasePending      = "Pending"
	PhaseAIValidating = "AIValidating"
	PhaseFabricPend   = "FabricPending"
	PhaseEndorsed     = "Endorsed"
	PhaseApproved     = "Approved"
	PhaseRejected     = "Rejected"
	PhaseApplied      = "Applied"
	PhaseFailed       = "Failed"
)

// AnnotationProofToken é a annotation injetada nos manifests aprovados.
// O ValidatingWebhook verifica essa annotation em cada resource admission.
const AnnotationProofToken = "kubechain.io/proof-token"

// AnnotationFabricTxID referencia a transação no Fabric para auditoria.
const AnnotationFabricTxID = "kubechain.io/fabric-txid"
