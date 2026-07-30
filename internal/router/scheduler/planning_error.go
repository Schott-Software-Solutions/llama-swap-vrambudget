package scheduler

// PlanningErrorSwapper is an optional extension for planners whose decisions
// can fail because of an internal invariant or dynamic input inconsistency.
// Existing Swapper implementations do not need to implement it.
type PlanningErrorSwapper interface {
	PlanningErrorFor(target string, running []string) error
}

func swapperPlanningError(planner Swapper, target string, running []string) error {
	if errorPlanner, ok := planner.(PlanningErrorSwapper); ok {
		return errorPlanner.PlanningErrorFor(target, running)
	}
	return nil
}
