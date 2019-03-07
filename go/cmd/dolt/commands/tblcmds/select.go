package tblcmds

import (
	"errors"
	"github.com/attic-labs/noms/go/types"
	"github.com/fatih/color"
	"github.com/liquidata-inc/ld/dolt/go/cmd/dolt/cli"
	"github.com/liquidata-inc/ld/dolt/go/cmd/dolt/commands"
	"github.com/liquidata-inc/ld/dolt/go/cmd/dolt/errhand"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/doltdb"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/env"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/row"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/rowconv"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/schema"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/table/pipeline"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/table/typed"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/table/typed/noms"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/table/untyped"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/table/untyped/csv"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/table/untyped/fwt"
	"github.com/liquidata-inc/ld/dolt/go/libraries/utils/argparser"
	"github.com/liquidata-inc/ld/dolt/go/libraries/utils/iohelp"
	"strings"
)

const (
	whereParam        = "where"
	limitParam        = "limit"
	hideConflictsFlag = "hide-conflicts"
	defaultLimit      = -1
	cnfColName        = "Cnf"
)

var cnfTag = schema.ReservedTagMin

var selShortDesc = "print a selection of a table"
var selLongDesc = `The dolt table select command selects rows from a table and prints out some or all of the table's columns`
var selSynopsis = []string{
	"[--limit <record_count>] [--where <col1=val1>] [--hide-conflicts] [<commit>] <table> [<column>...]",
}

type whereFunc func(r row.Row) (isTrue bool)

func parseWhere(sch schema.Schema, whereClause string) (whereFunc, error) {
	if whereClause == "" {
		return func(r row.Row) bool {
			return true
		}, nil
	} else {
		tokens := strings.Split(whereClause, "=")

		if len(tokens) != 2 {
			return nil, errors.New("'" + whereClause + "' is not in the format key=value")
		}

		key := tokens[0]
		valStr := tokens[1]

		col, ok := sch.GetAllCols().GetByName(key)

		if !ok {
			return nil, errors.New("where clause is invalid. '" + key + "' is not a known column.")
		}

		tag := col.Tag
		convFunc := doltcore.GetConvFunc(types.StringKind, col.Kind)
		val, err := convFunc(types.String(valStr))

		if err != nil {
			return nil, errors.New("unable to convert '" + valStr + "' to " + col.KindString())
		}

		return func(r row.Row) bool {
			rowVal, ok := r.GetColVal(tag)

			if !ok {
				return false
			}

			return val.Equals(rowVal)
		}, nil
	}
}

type SelectTransform struct {
	p      *pipeline.Pipeline
	whFunc whereFunc
	limit  int
	count  int
}

func (st *SelectTransform) LimitAndFilter(inRow row.Row, props pipeline.ReadableMap) ([]*pipeline.TransformedRowResult, string) {
	if st.limit == -1 || st.count < st.limit {
		if st.whFunc(inRow) {
			st.count++
			return []*pipeline.TransformedRowResult{{inRow, nil}}, ""
		}
	} else {
		st.p.NoMore()
	}

	return nil, ""
}

type SelectArgs struct {
	tblName       string
	colNames      []string
	whereClause   string
	limit         int
	hideConflicts bool
}

func Select(commandStr string, args []string, dEnv *env.DoltEnv) int {
	ap := argparser.NewArgParser()
	ap.ArgListHelp["table"] = "List of tables to be printed."
	ap.ArgListHelp["column"] = "List of columns to be printed"
	ap.SupportsString(whereParam, "", "column", "")
	ap.SupportsInt(limitParam, "", "record_count", "")
	ap.SupportsFlag(hideConflictsFlag, "", "")
	help, usage := cli.HelpAndUsagePrinters(commandStr, selShortDesc, selLongDesc, selSynopsis, ap)
	apr := cli.ParseArgs(ap, args, help)
	args = apr.Args()

	if len(args) == 0 {
		usage()
		return 1
	}

	root, verr := commands.GetWorkingWithVErr(dEnv)

	if verr == nil {
		var cm *doltdb.Commit
		cm, verr = commands.MaybeGetCommitWithVErr(dEnv, args[0])

		if verr == nil {
			if cm != nil {
				args = args[1:]
				root = cm.GetRootValue()
			}

			if len(args) == 0 {
				cli.Println("No tables specified")
				usage()
				return 1
			}

			tblName := args[0]

			var colNames []string
			if len(args) > 1 {
				colNames = args[1:]
			}

			selArgs := &SelectArgs{
				tblName,
				colNames,
				apr.GetValueOrDefault(whereParam, ""),
				apr.GetIntOrDefault(limitParam, defaultLimit),
				apr.Contains(hideConflictsFlag)}

			verr = printTable(root, selArgs)
		}
	}

	if verr != nil {
		cli.PrintErrln(verr.Verbose())
		return 1
	}

	return 0
}

func printTable(root *doltdb.RootValue, selArgs *SelectArgs) errhand.VerboseError {
	var verr errhand.VerboseError
	if !root.HasTable(selArgs.tblName) {
		return errhand.BuildDError("error: unknown table '%s'", selArgs.tblName).Build()
	}

	tbl, _ := root.GetTable(selArgs.tblName)
	tblSch := tbl.GetSchema()
	whereFunc, err := parseWhere(tblSch, selArgs.whereClause)

	if err != nil {
		return errhand.BuildDError("error: failed to parse where cause").AddCause(err).SetPrintUsage().Build()
	}

	selTrans := &SelectTransform{nil, whereFunc, selArgs.limit, 0}
	transforms := pipeline.NewTransformCollection(pipeline.NewNamedTransform("select", selTrans.LimitAndFilter))
	sch := maybeAddCnfColTransform(transforms, tbl, tblSch)
	outSch, verr := addMapTransform(selArgs, sch, transforms)

	if verr != nil {
		return verr
	}

	addSizingTransform(outSch, transforms)

	rd := noms.NewNomsMapReader(tbl.GetRowData(), tblSch)
	defer rd.Close()

	wr, _ := csv.NewCSVWriter(iohelp.NopWrCloser(cli.CliOut), outSch, &csv.CSVFileInfo{Delim: '|'})
	defer wr.Close()

	badRowCB := func(tff *pipeline.TransformRowFailure) (quit bool) {
		cli.PrintErrln(color.RedString("error: failed to transform row %s.", row.Fmt(tff.Row, outSch)))
		return true
	}

	rdProcFunc := pipeline.ProcFuncForReader(rd)
	wrProcFunc := pipeline.ProcFuncForWriter(wr)
	p := pipeline.NewAsyncPipeline(rdProcFunc, wrProcFunc, transforms, badRowCB)
	selTrans.p = p

	colNames := schema.ExtractAllColNames(outSch)
	p.InsertRow("fwt", untyped.NewRowFromTaggedStrings(outSch, colNames))

	p.Start()
	p.Wait()

	return verr
}

func addSizingTransform(outSch schema.Schema, transforms *pipeline.TransformCollection) {
	autoSizeTransform := fwt.NewAutoSizingFWTTransformer(outSch, fwt.PrintAllWhenTooLong, 10000)
	transforms.AppendTransforms(pipeline.NamedTransform{"fwt", autoSizeTransform.TransformToFWT})
}

func addMapTransform(selArgs *SelectArgs, sch schema.Schema, transforms *pipeline.TransformCollection) (schema.Schema, errhand.VerboseError) {
	colColl := sch.GetAllCols()

	if len(selArgs.colNames) > 0 {
		cols := make([]schema.Column, 0, len(selArgs.colNames)+1)

		if !selArgs.hideConflicts {
			if col, ok := sch.GetAllCols().GetByName(cnfColName); ok {
				cols = append(cols, col)
			}
		}

		for _, name := range selArgs.colNames {
			if col, ok := colColl.GetByName(name); !ok {
				return nil, errhand.BuildDError("error: unknown column '%s'", name).Build()
			} else {
				cols = append(cols, col)
			}
		}

		colColl, _ = schema.NewColCollection(cols...)
	}

	outSch := schema.SchemaFromCols(colColl)
	mapping, err := rowconv.TagMapping(sch, untyped.UntypeSchema(outSch))

	if err != nil {
		panic(err)
	}

	rConv, _ := rowconv.NewRowConverter(mapping)
	transform := pipeline.NewNamedTransform("map", rowconv.GetRowConvTransformFunc(rConv))
	transforms.AppendTransforms(transform)

	return mapping.DestSch, nil
}

func maybeAddCnfColTransform(transColl *pipeline.TransformCollection, tbl *doltdb.Table, tblSch schema.Schema) schema.Schema {
	if tbl.HasConflicts() {
		// this is so much code to add a column
		const transCnfSetName = "set cnf col"

		_, confSchema := untyped.NewUntypedSchemaWithFirstTag(cnfTag, cnfColName)
		schWithConf, _ := typed.TypedSchemaUnion(confSchema, tblSch)

		_, confData, _ := tbl.GetConflicts()

		cnfTransform := pipeline.NewNamedTransform(transCnfSetName, CnfTransformer(tblSch, schWithConf, confData))
		transColl.AppendTransforms(cnfTransform)

		return schWithConf
	}

	return tblSch
}

var confLabel = types.String(" ! ")
var noConfLabel = types.String("   ")

func CnfTransformer(inSch, outSch schema.Schema, conflicts types.Map) func(inRow row.Row, props pipeline.ReadableMap) (rowData []*pipeline.TransformedRowResult, badRowDetails string) {
	return func(inRow row.Row, props pipeline.ReadableMap) ([]*pipeline.TransformedRowResult, string) {
		key := inRow.NomsMapKey(inSch)

		var err error
		if conflicts.Has(key) {
			inRow, err = inRow.SetColVal(cnfTag, confLabel, outSch)
		} else {
			inRow, err = inRow.SetColVal(cnfTag, noConfLabel, outSch)
		}

		if err != nil {
			panic(err)
		}

		return []*pipeline.TransformedRowResult{{inRow, nil}}, ""
	}
}
