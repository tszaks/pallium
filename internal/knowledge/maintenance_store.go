package knowledge

import "github.com/tszaks/pallium/internal/workflow"

type MaintenanceStore = workflow.KnowledgeMaintenanceStore
type MaintenanceStatus = workflow.MaintenanceStatus
type Registration = workflow.Registration

var ErrCallBudget = workflow.ErrCallBudget
var ErrCallSlots = workflow.ErrCallSlots

func OpenMaintenance() (*MaintenanceStore, error) { return workflow.OpenKnowledgeMaintenance() }
func openMaintenancePath(path string) (*MaintenanceStore, error) {
	return workflow.OpenKnowledgeMaintenancePath(path)
}
