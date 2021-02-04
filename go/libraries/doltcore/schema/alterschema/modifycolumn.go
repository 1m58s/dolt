// Copyright 2019 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package alterschema

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/row"
	"github.com/dolthub/dolt/go/libraries/doltcore/schema"
	"github.com/dolthub/dolt/go/store/types"
)

// ModifyColumn modifies the column with the name given, replacing it with the new definition provided. A column with
// the name given must exist in the schema of the table.
func ModifyColumn(
	ctx context.Context,
	tbl *doltdb.Table,
	existingCol schema.Column,
	newCol schema.Column,
	order *ColumnOrder,
) (*doltdb.Table, error) {

	sch, err := tbl.GetSchema(ctx)
	if err != nil {
		return nil, err
	}

	if err := validateModifyColumn(ctx, tbl, existingCol, newCol); err != nil {
		return nil, err
	}

	// Modify statements won't include key info, so fill it in from the old column
	if existingCol.IsPartOfPK {
		newCol.IsPartOfPK = true
	}

	newSchema, err := replaceColumnInSchema(sch, existingCol, newCol, order)
	if err != nil {
		return nil, err
	}

	updatedTable, err := tbl.UpdateSchema(ctx, newSchema)
	if err != nil {
		return nil, err
	}

	updatedTable, err = handleNotNullConstraint(ctx, updatedTable, newSchema, existingCol, newCol)
	if err != nil {
		return nil, err
	}

	return updatedTable, nil
}

// validateModifyColumn returns an error if the column as specified cannot be added to the schema given.
func validateModifyColumn(ctx context.Context, tbl *doltdb.Table, existingCol schema.Column, modifiedCol schema.Column) error {
	sch, err := tbl.GetSchema(ctx)
	if err != nil {
		return err
	}

	if existingCol.Kind != modifiedCol.Kind || !existingCol.TypeInfo.Equals(modifiedCol.TypeInfo) {
		return errors.New("unsupported feature: column types cannot be changed")
	}

	cols := sch.GetAllCols()
	err = cols.Iter(func(currColTag uint64, currCol schema.Column) (stop bool, err error) {
		if currColTag == modifiedCol.Tag {
			return false, nil
		} else if strings.ToLower(currCol.Name) == strings.ToLower(modifiedCol.Name) {
			return true, fmt.Errorf("A column with the name %s already exists.", modifiedCol.Name)
		}

		return false, nil
	})

	if err != nil {
		return err
	}

	return nil
}

// handleNotNullConstraint validates that rows do not violate a NOT NULL constraint, if one exists, and
// rebuild indexes on the modified column if necessary
func handleNotNullConstraint(ctx context.Context, tbl *doltdb.Table, newSchema schema.Schema, oldCol, newCol schema.Column) (*doltdb.Table, error) {
	rowData, err := tbl.GetRowData(ctx)
	if err != nil {
		return nil, err
	}

	// Iterate over the rows in the table, checking for nils (illegal if the column is declared not null)
	if !newCol.IsNullable() {
		err = rowData.Iter(ctx, func(key, value types.Value) (stop bool, err error) {
			r, err := row.FromNoms(newSchema, key.(types.Tuple), value.(types.Tuple))
			if err != nil {
				return false, err
			}
			val, ok := r.GetColVal(newCol.Tag)
			if (!ok || val == nil) && !newCol.IsNullable() {
				return true, fmt.Errorf("cannot change column to NOT NULL when one or more values is NULL")
			}

			return false, nil
		})
	}

	if !newCol.TypeInfo.Equals(oldCol.TypeInfo) ||
		newCol.IsNullable() != oldCol.IsNullable() {

		for _, index := range newSchema.Indexes().IndexesWithTag(oldCol.Tag) {
			rebuiltIndexData, err := editor.RebuildIndex(ctx, tbl, index.Name())
			if err != nil {
				return nil, err
			}

			tbl, err = tbl.SetIndexRowData(ctx, index.Name(), rebuiltIndexData)
			if err != nil {
				return nil, err
			}
		}
	}

	return tbl, err
}

// replaceColumnInSchema replaces the column with the name given with its new definition, optionally reordering it.
func replaceColumnInSchema(sch schema.Schema, oldCol schema.Column, newCol schema.Column, order *ColumnOrder) (schema.Schema, error) {
	// If no order is specified, insert in the same place as the existing column
	if order == nil {
		prevColumn := ""
		sch.GetAllCols().Iter(func(tag uint64, col schema.Column) (stop bool, err error) {
			if col.Name == oldCol.Name {
				if prevColumn == "" {
					order = &ColumnOrder{First: true}
				}
				return true, nil
			} else {
				prevColumn = col.Name
			}
			return false, nil
		})

		if order == nil {
			if prevColumn != "" {
				order = &ColumnOrder{After: prevColumn}
			} else {
				return nil, fmt.Errorf("Couldn't find column %s", oldCol.Name)
			}
		}
	}

	var newCols []schema.Column
	if order.First {
		newCols = append(newCols, newCol)
	}
	sch.GetAllCols().Iter(func(tag uint64, col schema.Column) (stop bool, err error) {
		if col.Name != oldCol.Name {
			newCols = append(newCols, col)
		}

		if order.After == col.Name {
			newCols = append(newCols, newCol)
		}

		return false, nil
	})

	collection := schema.NewColCollection(newCols...)

	err := schema.ValidateForInsert(collection)
	if err != nil {
		return nil, err
	}

	newSch, err := schema.SchemaFromCols(collection)
	if err != nil {
		return nil, err
	}
	newSch.Indexes().AddIndex(sch.Indexes().AllIndexes()...)
	return newSch, nil
}
