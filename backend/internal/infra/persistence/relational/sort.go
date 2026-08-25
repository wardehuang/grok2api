package relational

import (
	"strings"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type sortSpec struct {
	expression       string
	nullsLast        bool
	defaultDirection repository.SortDirection
	tieDirection     repository.SortDirection
}

func applyStableSort(query *gorm.DB, sort repository.SortQuery, fields map[string]sortSpec, fallback sortSpec, idColumn string) *gorm.DB {
	spec, ok := fields[sort.Field]
	if !ok || strings.TrimSpace(spec.expression) == "" {
		spec = fallback
	}
	direction := "ASC"
	resolvedDirection := sort.Direction
	if resolvedDirection != repository.SortAscending && resolvedDirection != repository.SortDescending {
		resolvedDirection = spec.defaultDirection
	}
	if resolvedDirection == repository.SortDescending {
		direction = "DESC"
	}
	if spec.nullsLast {
		query = query.Order("CASE WHEN " + spec.expression + " IS NULL THEN 1 ELSE 0 END ASC")
	}
	tieDirection := resolvedDirection
	if spec.tieDirection == repository.SortAscending || spec.tieDirection == repository.SortDescending {
		tieDirection = spec.tieDirection
	}
	tieSQLDirection := "ASC"
	if tieDirection == repository.SortDescending {
		tieSQLDirection = "DESC"
	}
	return query.Order(spec.expression + " " + direction).Order(idColumn + " " + tieSQLDirection)
}

func stableSortSpec(sort repository.SortQuery, fields map[string]sortSpec, fallback sortSpec) (sortSpec, string) {
	spec, ok := fields[sort.Field]
	if !ok || strings.TrimSpace(spec.expression) == "" {
		spec = fallback
	}
	direction := "ASC"
	resolvedDirection := sort.Direction
	if resolvedDirection != repository.SortAscending && resolvedDirection != repository.SortDescending {
		resolvedDirection = spec.defaultDirection
	}
	if resolvedDirection == repository.SortDescending {
		direction = "DESC"
	}
	return spec, direction
}
