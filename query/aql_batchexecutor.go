package query

import (
	"fmt"
	"github.com/uber/aresdb/memutils"
	queryCom "github.com/uber/aresdb/query/common"
	"github.com/uber/aresdb/utils"
	"time"
	"unsafe"
)

// BatchExecutor is batch executor interface for both Non-aggregation query and Aggregation query
type BatchExecutor interface {
	// filter operation
	filter()
	// join operation
	join()
	// project of measure/select columns
	project()
	// reduce to sort and aggregate result
	reduce()
	// main method to call for execution
	Run(isLastBatch bool)
}

// DummyBatchExecutorImpl is a dummy executor which do nothing
type DummyBatchExecutorImpl struct {
}

func NewDummyBatchExecutor() BatchExecutor {
	return &DummyBatchExecutorImpl{}
}

func (e *DummyBatchExecutorImpl) filter() {
}

func (e *DummyBatchExecutorImpl) join() {
}

func (e *DummyBatchExecutorImpl) project() {
}

func (e *DummyBatchExecutorImpl) reduce() {
}

// Run is dummy fuction for dummy executor
func (e *DummyBatchExecutorImpl) Run(isLastBatch bool) {
}

// BatchExecutorImpl is batch executor implementation for original aggregation query
type BatchExecutorImpl struct {
	qc                  *AQLQueryContext
	batchID             int32
	isLastBatch         bool
	customFilterFunc    customFilterExecutor
	stream              unsafe.Pointer
	start               time.Time
	sizeBeforeGeoFilter int
}



func NewBatchExecutor(qc *AQLQueryContext, batchID int32, customFilterFunc customFilterExecutor, stream unsafe.Pointer) BatchExecutor {
	if qc.isNonAggregationQuery {
		return &NonAggrBatchExecutorImpl{
			BatchExecutorImpl: &BatchExecutorImpl{
				qc:               qc,
				batchID:          batchID,
				customFilterFunc: customFilterFunc,
				stream:           stream,
			},
		}
	}

	return &BatchExecutorImpl{
		qc:               qc,
		batchID:          batchID,
		customFilterFunc: customFilterFunc,
		stream:           stream,
	}
}

func (e *BatchExecutorImpl) filter() {
	// process main table common filter
	e.qc.doProfile(func() {
		for _, filter := range e.qc.OOPK.MainTableCommonFilters {
			e.qc.OOPK.currentBatch.processExpression(filter, nil,
				e.qc.TableScanners, e.qc.OOPK.foreignTables, e.stream, e.qc.Device, e.qc.OOPK.currentBatch.filterAction)
		}
		e.customFilterFunc(e.stream)
		e.qc.reportTimingForCurrentBatch(e.stream, &e.start, filterEvalTiming)
	}, "filters", e.stream)
}

func (e *BatchExecutorImpl) join() {
	e.qc.doProfile(func() {
		// join foreign tables
		for joinTableID, foreignTable := range e.qc.OOPK.foreignTables {
			if foreignTable != nil {
				// prepare foreign table recordIDs
				// Note:
				// RecordID {
				//   int32_t batchID
				// 	 uint32_t index
				// }
				// takes up 8 bytes
				e.qc.OOPK.currentBatch.foreignTableRecordIDsD = append(e.qc.OOPK.currentBatch.foreignTableRecordIDsD, deviceAllocate(8*e.qc.OOPK.currentBatch.size, e.qc.Device))
				mainTableJoinColumnIndex := e.qc.TableScanners[0].ColumnsByIDs[foreignTable.remoteJoinColumn.ColumnID]
				// perform hash lookup
				e.qc.OOPK.currentBatch.prepareForeignRecordIDs(mainTableJoinColumnIndex, joinTableID, *foreignTable, e.stream, e.qc.Device)
			}
		}
		e.qc.reportTimingForCurrentBatch(e.stream, &e.start, prepareForeignRecordIDsTiming)
	}, "joins", e.stream)

	e.qc.doProfile(func() {
		// process filters that involves foreign table columns if any
		for _, filter := range e.qc.OOPK.ForeignTableCommonFilters {
			e.qc.OOPK.currentBatch.processExpression(filter, nil,
				e.qc.TableScanners, e.qc.OOPK.foreignTables, e.stream, e.qc.Device, e.qc.OOPK.currentBatch.filterAction)
		}
		e.qc.reportTimingForCurrentBatch(e.stream, &e.start, foreignTableFilterEvalTiming)
	}, "filters", e.stream)

	if e.qc.OOPK.geoIntersection != nil {
		// allocate two predicate vector for geo intersect
		numWords := (e.qc.OOPK.geoIntersection.numShapes + 31) / 32
		e.qc.OOPK.currentBatch.geoPredicateVectorD = deviceAllocate(e.qc.OOPK.currentBatch.size*4*numWords, e.qc.Device)
	}

	e.sizeBeforeGeoFilter = e.qc.OOPK.currentBatch.size
	e.qc.doProfile(func() {
		if e.qc.OOPK.geoIntersection != nil {
			pointColumnIndex := e.qc.TableScanners[e.qc.OOPK.geoIntersection.pointTableID].
				ColumnsByIDs[e.qc.OOPK.geoIntersection.pointColumnID]
			e.qc.OOPK.currentBatch.geoIntersect(
				e.qc.OOPK.geoIntersection,
				pointColumnIndex,
				e.qc.OOPK.foreignTables,
				e.qc.OOPK.currentBatch.geoPredicateVectorD,
				e.stream, e.qc.Device)
		}
		e.qc.reportTimingForCurrentBatch(e.stream, &e.start, geoIntersectEvalTiming)
	}, "geo_intersect", e.stream)
}

func (e *BatchExecutorImpl) project() {
	// Prepare for dimension and measure evaluation.
	e.qc.OOPK.currentBatch.prepareForDimAndMeasureEval(e.qc.OOPK.DimRowBytes, e.qc.OOPK.MeasureBytes, e.qc.OOPK.NumDimsPerDimWidth, e.qc.OOPK.IsHLL(), e.stream)

	e.qc.reportTimingForCurrentBatch(e.stream, &e.start, prepareForDimAndMeasureTiming)

	// dimension expression evaluation.
	for dimIndex, dimension := range e.qc.OOPK.Dimensions {
		e.qc.doProfile(func() {
			dimVectorIndex := e.qc.OOPK.DimensionVectorIndex[dimIndex]
			dimValueOffset, dimNullOffset := queryCom.GetDimensionStartOffsets(e.qc.OOPK.NumDimsPerDimWidth, dimVectorIndex, e.qc.OOPK.currentBatch.resultCapacity)
			if e.qc.OOPK.geoIntersection != nil && e.qc.OOPK.geoIntersection.dimIndex == dimIndex {
				e.qc.OOPK.currentBatch.writeGeoShapeDim(
					e.qc.OOPK.geoIntersection, e.qc.OOPK.currentBatch.geoPredicateVectorD,
					dimValueOffset, dimNullOffset, e.sizeBeforeGeoFilter, e.stream, e.qc.Device)
			} else {
				dimensionExprRootAction := e.qc.OOPK.currentBatch.makeWriteToDimensionVectorAction(dimValueOffset, dimNullOffset)
				e.qc.OOPK.currentBatch.processExpression(dimension, nil,
					e.qc.TableScanners, e.qc.OOPK.foreignTables, e.stream, e.qc.Device, dimensionExprRootAction)
			}
		}, fmt.Sprintf("dim%d", dimIndex), e.stream)
	}

	e.qc.reportTimingForCurrentBatch(e.stream, &e.start, dimEvalTiming)

	// measure evaluation.
	e.qc.doProfile(func() {
		measureExprRootAction := e.qc.OOPK.currentBatch.makeWriteToMeasureVectorAction(e.qc.OOPK.AggregateType, e.qc.OOPK.MeasureBytes)
		e.qc.OOPK.currentBatch.processExpression(e.qc.OOPK.Measure, nil, e.qc.TableScanners, e.qc.OOPK.foreignTables, e.stream, e.qc.Device, measureExprRootAction)
		e.qc.reportTimingForCurrentBatch(e.stream, &e.start, measureEvalTiming)
	}, "measure", e.stream)

	// wait for stream to clean up non used buffer before final aggregation
	memutils.WaitForCudaStream(e.stream, e.qc.Device)
	e.qc.OOPK.currentBatch.cleanupBeforeAggregation()
}

func (e *BatchExecutorImpl) reduce() {
	// init dimIndexVectorD for sorting and reducing
	if e.qc.OOPK.IsHLL() {
		initIndexVector(e.qc.OOPK.currentBatch.dimIndexVectorD[0].getPointer(), 0, e.qc.OOPK.currentBatch.resultSize, e.stream, e.qc.Device)
		initIndexVector(e.qc.OOPK.currentBatch.dimIndexVectorD[1].getPointer(), e.qc.OOPK.currentBatch.resultSize, e.qc.OOPK.currentBatch.resultSize+e.qc.OOPK.currentBatch.size, e.stream, e.qc.Device)
	} else {
		initIndexVector(e.qc.OOPK.currentBatch.dimIndexVectorD[0].getPointer(), 0, e.qc.OOPK.currentBatch.resultSize+e.qc.OOPK.currentBatch.size, e.stream, e.qc.Device)
	}

	if e.qc.OOPK.IsHLL() {
		e.qc.doProfile(func() {
			e.qc.OOPK.hllVectorD, e.qc.OOPK.hllDimRegIDCountD, e.qc.OOPK.hllVectorSize =
				e.qc.OOPK.currentBatch.hll(e.qc.OOPK.NumDimsPerDimWidth, e.isLastBatch, e.stream, e.qc.Device)
			e.qc.reportTimingForCurrentBatch(e.stream, &e.start, hllEvalTiming)
		}, "hll", e.stream)
	} else {
		// sort by key.
		e.qc.doProfile(func() {
			e.qc.OOPK.currentBatch.sortByKey(e.qc.OOPK.NumDimsPerDimWidth, e.qc.OOPK.MeasureBytes, e.stream, e.qc.Device)
			e.qc.reportTimingForCurrentBatch(e.stream, &e.start, sortEvalTiming)
		}, "sort", e.stream)

		// reduce by key.
		e.qc.doProfile(func() {
			e.qc.OOPK.currentBatch.reduceByKey(e.qc.OOPK.NumDimsPerDimWidth, e.qc.OOPK.MeasureBytes, e.qc.OOPK.AggregateType, e.stream, e.qc.Device)
			e.qc.reportTimingForCurrentBatch(e.stream, &e.start, reduceEvalTiming)
		}, "reduce", e.stream)
	}
	memutils.WaitForCudaStream(e.stream, e.qc.Device)
}

func (e *BatchExecutorImpl) Run(isLastBatch bool) {
	e.isLastBatch = isLastBatch
	start := utils.Now()
	// initialize index vector.
	initIndexVector(e.qc.OOPK.currentBatch.indexVectorD.getPointer(), 0, e.qc.OOPK.currentBatch.size, e.stream, e.qc.Device)

	e.qc.reportTimingForCurrentBatch(e.stream, &start, initIndexVectorTiming)

	e.filter()

	e.join()

	e.project()

	e.reduce()

	// swap result buffer before next batch
	e.qc.OOPK.currentBatch.swapResultBufferForNextBatch()
	e.qc.reportTimingForCurrentBatch(e.stream, &start, cleanupTiming)
	e.qc.reportBatch(e.batchID > 0)

	// Only profile one batch.
	e.qc.Profiling = ""
}
