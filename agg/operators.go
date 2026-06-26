package agg

import (
	"math"

	"github.com/tamnd/doc/bson"
)

// opTable maps each aggregation operator name to its compiler. It is the single
// dispatch point used by compileOperator (spec 2061 doc 12 §4). It is filled in
// init to break the initialization cycle: the compilers recurse into
// compileOperator, which reads opTable.
var opTable map[string]opCompiler

func init() {
	opTable = map[string]opCompiler{
		// arithmetic
		"$add":      eager(0, -1, opAdd),
		"$subtract": eager(2, 2, opSubtract),
		"$multiply": eager(0, -1, opMultiply),
		"$divide":   eager(2, 2, opDivide),
		"$mod":      eager(2, 2, opMod),
		"$abs":      unaryNum(opAbs),
		"$ceil":     unaryNum(opCeil),
		"$floor":    unaryNum(opFloor),
		"$round":    roundTrunc(true),
		"$trunc":    roundTrunc(false),
		"$pow":      eager(2, 2, opPow),
		"$sqrt":     unaryFloat(math.Sqrt),
		"$ln":       unaryFloat(math.Log),
		"$log10":    unaryFloat(math.Log10),
		"$log":      eager(2, 2, opLog),
		"$exp":      unaryFloat(math.Exp),

		// comparison
		"$eq":  compareOp(func(c int) bool { return c == 0 }),
		"$ne":  compareOp(func(c int) bool { return c != 0 }),
		"$gt":  compareOp(func(c int) bool { return c > 0 }),
		"$gte": compareOp(func(c int) bool { return c >= 0 }),
		"$lt":  compareOp(func(c int) bool { return c < 0 }),
		"$lte": compareOp(func(c int) bool { return c <= 0 }),
		"$cmp": eager(2, 2, opCmp),

		// boolean
		"$and": boolCompiler(true),
		"$or":  boolCompiler(false),
		"$not": eager(1, 1, opNot),

		// conditional
		"$cond":   compileCond,
		"$ifNull": compileIfNull,
		"$switch": compileSwitch,

		// string
		"$concat":       eager(1, -1, opConcat),
		"$substr":       eager(3, 3, opSubstrBytes),
		"$substrBytes":  eager(3, 3, opSubstrBytes),
		"$substrCP":     eager(3, 3, opSubstrCP),
		"$toLower":      caseFn(false),
		"$toUpper":      caseFn(true),
		"$strLenBytes":  strLen(false),
		"$strLenCP":     strLen(true),
		"$split":        eager(2, 2, opSplit),
		"$indexOfBytes": indexOf(false),
		"$indexOfCP":    indexOf(true),
		"$trim":         compileTrim(trimBoth),
		"$ltrim":        compileTrim(trimLeft),
		"$rtrim":        compileTrim(trimRight),
		"$replaceOne":   compileReplace(false),
		"$replaceAll":   compileReplace(true),
		"$regexMatch":   compileRegex(regexMatch),
		"$regexFind":    compileRegex(regexFindOne),
		"$regexFindAll": compileRegex(regexFindAll),

		// array
		"$size":          eager(1, 1, opSize),
		"$arrayElemAt":   eager(2, 2, opArrayElemAt),
		"$first":         endElem(false),
		"$last":          endElem(true),
		"$concatArrays":  eager(0, -1, opConcatArrays),
		"$slice":         eager(2, 3, opSlice),
		"$reverseArray":  eager(1, 1, opReverseArray),
		"$range":         eager(2, 3, opRange),
		"$in":            eager(2, 2, opIn),
		"$indexOfArray":  eager(2, 4, opIndexOfArray),
		"$isArray":       eager(1, 1, opIsArray),
		"$arrayToObject": eager(1, 1, opArrayToObject),
		"$objectToArray": eager(1, 1, opObjectToArray),
		"$sortArray":     compileSortArray,
		"$zip":           compileZip,
		"$filter":        compileFilter,
		"$map":           compileMap,
		"$reduce":        compileReduce,
		"$let":           compileLet,

		// set
		"$setEquals":       eager(2, -1, opSetEquals),
		"$setIntersection": eager(1, -1, opSetIntersection),
		"$setUnion":        eager(1, -1, opSetUnion),
		"$setDifference":   eager(2, 2, opSetDifference),
		"$setIsSubset":     eager(2, 2, opSetIsSubset),
		"$anyElementTrue":  eager(1, 1, opAnyElementTrue),
		"$allElementsTrue": eager(1, 1, opAllElementsTrue),

		// date
		"$year":           compileDatePart(partYear),
		"$month":          compileDatePart(partMonth),
		"$dayOfMonth":     compileDatePart(partDayOfMonth),
		"$hour":           compileDatePart(partHour),
		"$minute":         compileDatePart(partMinute),
		"$second":         compileDatePart(partSecond),
		"$millisecond":    compileDatePart(partMillisecond),
		"$dayOfYear":      compileDatePart(partDayOfYear),
		"$dayOfWeek":      compileDatePart(partDayOfWeek),
		"$isoDayOfWeek":   compileDatePart(partIsoDayOfWeek),
		"$isoWeek":        compileDatePart(partIsoWeek),
		"$isoWeekYear":    compileDatePart(partIsoWeekYear),
		"$week":           compileDatePart(partWeek),
		"$dateToString":   compileDateToString,
		"$dateFromString": compileDateFromString,
		"$dateAdd":        compileDateAdd(false),
		"$dateSubtract":   compileDateAdd(true),
		"$dateDiff":       compileDateDiff,
		"$dateToParts":    compileDateToParts,
		"$dateFromParts":  compileDateFromParts,
		"$dateTrunc":      compileDateTrunc,

		// type
		"$type":       eager(1, 1, opType),
		"$isNumber":   eager(1, 1, opIsNumber),
		"$convert":    compileConvert,
		"$toInt":      shorthandConvert(bson.TypeInt32),
		"$toLong":     shorthandConvert(bson.TypeInt64),
		"$toDouble":   shorthandConvert(bson.TypeDouble),
		"$toDecimal":  shorthandConvert(bson.TypeDecimal128),
		"$toString":   shorthandConvert(bson.TypeString),
		"$toBool":     shorthandConvert(bson.TypeBoolean),
		"$toDate":     shorthandConvert(bson.TypeDateTime),
		"$toObjectId": shorthandConvert(bson.TypeObjectID),

		// misc
		"$literal":      compileLiteral,
		"$mergeObjects": eager(1, -1, opMergeObjects),
		"$getField":     compileGetField,
		"$setField":     compileSetField,
		"$rand":         eager(0, 0, opRand),
	}
}
