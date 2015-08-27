package anomaly

import (
	"errors"
	"fmt"
	"github.com/ugorji/go/codec"
	"io"
	"math"
	"pfi/sensorbee/jubatus/internal/nearest"
	"pfi/sensorbee/sensorbee/data"
	"sync"
)

type LightLOF struct {
	nn     nearest.Neighbor
	nnNum  int
	rnnNum int

	kdists []float32
	lrds   []float32

	m sync.RWMutex
}

const (
	InvalidNNAlgorithm = iota
	LSH
	Minhash
	EuclidLSH
)

type NNAlgorithm int

func NewLightLOF(nnAlgo NNAlgorithm, hashNum, nnNum, rnnNum int) (*LightLOF, error) {
	if hashNum <= 0 {
		return nil, errors.New("number of hash bits must be greater than zero")
	}
	if nnNum <= 1 {
		return nil, errors.New("number of nearest neighbor must be greater than one")
	}
	if rnnNum < nnNum {
		return nil, errors.New("number of reverse nearest neighbor must be greater than or equal to number of nearest neighbor")
	}

	var nn nearest.Neighbor
	switch nnAlgo {
	case LSH:
		nn = nearest.NewLSH(hashNum)
	case Minhash:
		nn = nearest.NewMinhash(hashNum)
	case EuclidLSH:
		nn = nearest.NewEuclidLSH(hashNum)
	default:
		return nil, errors.New("invalid nearest neighbor algorithm")
	}

	return &LightLOF{
		nn:     nn,
		nnNum:  nnNum,
		rnnNum: rnnNum,
	}, nil
}

const (
	lightLOFFormatVersion = 1
)

type lightLOFMsgpack struct {
	_struct struct{} `codec:",toarray"`

	NNNum  int
	RNNNum int

	KDists []float32
	LRDs   []float32
}

func (l *LightLOF) Save(w io.Writer) error {
	l.m.RLock()
	defer l.m.RUnlock()

	if _, err := w.Write([]byte{lightLOFFormatVersion}); err != nil {
		return err
	}

	enc := codec.NewEncoder(w, anomalyMsgpackHandle)
	if err := enc.Encode(&lightLOFMsgpack{
		NNNum:  l.nnNum,
		RNNNum: l.rnnNum,

		KDists: l.kdists,
		LRDs:   l.lrds,
	}); err != nil {
		return err
	}
	return nearest.Save(l.nn, w)
}

func LoadLightLOF(r io.Reader) (*LightLOF, error) {
	formatVersion := make([]byte, 1)
	if _, err := r.Read(formatVersion); err != nil {
		return nil, err
	}

	switch formatVersion[0] {
	case 1:
		return loadLightLOFFormatV1(r)
	default:
		return nil, fmt.Errorf("unsupported format version of LightLOF container: %v", formatVersion[0])
	}
}

func loadLightLOFFormatV1(r io.Reader) (*LightLOF, error) {
	m := lightLOFMsgpack{}
	dec := codec.NewDecoder(r, anomalyMsgpackHandle)
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	nn, err := nearest.Load(r)
	if err != nil {
		return nil, err
	}

	return &LightLOF{
		nn:     nn,
		nnNum:  m.NNNum,
		rnnNum: m.RNNNum,

		kdists: m.KDists,
		lrds:   m.LRDs,
	}, nil
}

func (l *LightLOF) Add(v FeatureVector) (score float32, err error) {
	nnfv, err := v.toNNFV()
	if err != nil {
		return 0, err
	}

	l.m.Lock()
	defer l.m.Unlock()

	id := l.add(nnfv)
	score = l.calcScoreByID(id)
	return score, nil
}

func (l *LightLOF) AddWithoutCalcScore(v FeatureVector) error {
	nnfv, err := v.toNNFV()
	if err != nil {
		return err
	}

	l.m.Lock()
	defer l.m.Unlock()

	l.add(nnfv)
	return nil
}

func (l *LightLOF) add(v nearest.FeatureVector) ID {
	l.kdists = append(l.kdists, 0)
	l.lrds = append(l.lrds, 0)
	l.setRow(v)

	return ID(len(l.kdists))
}

func (l *LightLOF) CalcScore(v FeatureVector) (float32, error) {
	nnFV, err := v.toNNFV()
	if err != nil {
		return 0, err
	}

	l.m.RLock()
	defer l.m.RUnlock()

	lrd, neighborLRDs := l.collectLRDs(nnFV)
	return calcLOF(lrd, neighborLRDs), nil
}

func (l *LightLOF) calcScoreByID(id ID) float32 {
	lrd, neighborLRDs := l.collectLRDsByID(id)
	return calcLOF(lrd, neighborLRDs)
}

func (l *LightLOF) setRow(v nearest.FeatureVector) {
	nnID := nearest.ID(len(l.kdists))
	l.nn.SetRow(nnID, v)

	neighbors := l.nn.NeighborRowFromID(nnID, l.rnnNum)

	nestedNeighbors := map[ID][]nearest.IDist{}
	for i := range neighbors {
		nnID := neighbors[i].ID
		id := ID(nnID)
		nnResult := l.nn.NeighborRowFromID(nnID, l.nnNum)
		nestedNeighbors[id] = nnResult
		l.kdists[id-1] = nnResult[len(nnResult)-1].Dist
	}

	for i := range neighbors {
		nnID := neighbors[i].ID
		id := ID(nnID)
		nn := nestedNeighbors[id]
		var lrd float32 = 1
		if len(nn) > 0 {
			length := minInt(len(nn), l.nnNum)
			var sumReachability float32
			for i := 0; i < length; i++ {
				sumReachability += maxFloat32(nn[i].Dist, l.kdists[nn[i].ID-1])
			}
			if sumReachability == 0 {
				lrd = inf32
			} else {
				lrd = float32(length) / sumReachability
			}
		}
		l.lrds[id-1] = lrd
	}
}

func (l *LightLOF) collectLRDs(v nearest.FeatureVector) (float32, []float32) {
	neighbors := l.nn.NeighborRowFromFV(v, l.nnNum)
	if len(neighbors) == 0 {
		return inf32, nil
	}

	return l.collectLRDsImpl(neighbors)
}

func (l *LightLOF) collectLRDsByID(id ID) (float32, []float32) {
	nnID := nearest.ID(id)
	neighbors := l.nn.NeighborRowFromID(nearest.ID(id), l.nnNum+1)
	if len(neighbors) == 0 {
		return inf32, nil
	}
	for i := range neighbors {
		if neighbors[i].ID == nnID {
			copy(neighbors[1:i+1], neighbors[0:])
			neighbors = neighbors[1:]
			break
		}
	}
	if len(neighbors) > l.nnNum {
		neighbors = neighbors[:l.nnNum]
	}
	if len(neighbors) == 0 {
		return inf32, nil
	}

	return l.collectLRDsImpl(neighbors)
}

func (l *LightLOF) collectLRDsImpl(neighbors []nearest.IDist) (float32, []float32) {
	neighborKDists := make([]float32, len(neighbors))
	neighborLRDs := make([]float32, len(neighbors))

	for i := range neighbors {
		id := ID(neighbors[i].ID)
		neighborKDists[i] = l.kdists[id-1]
		neighborLRDs[i] = l.lrds[id-1]
	}

	var sumReachability float32
	for i := range neighbors {
		sumReachability += maxFloat32(neighbors[i].Dist, neighborKDists[i])
	}

	if sumReachability == 0 {
		return inf32, neighborLRDs
	}

	return float32(len(neighbors)) / sumReachability, neighborLRDs
}

func realloc(s []float32, n int) []float32 {
	ret := make([]float32, n)
	copy(ret, s)
	return ret
}

func calcLOF(lrd float32, neighborLRDs []float32) float32 {
	if len(neighborLRDs) == 0 {
		if lrd == 0 {
			return 1
		}
		return inf32
	}

	var sum float32
	for _, x := range neighborLRDs {
		sum += x
	}
	if isInf32(sum) && isInf32(lrd) {
		return 1
	}

	return sum / (float32(len(neighborLRDs)) * lrd)
}

type FeatureVector data.Map

func (v FeatureVector) toNNFV() (nearest.FeatureVector, error) {
	ret := make(nearest.FeatureVector, len(v))
	i := 0
	for k, v := range v {
		x, err := data.ToFloat(v)
		if err != nil {
			return nil, err
		}
		ret[i] = nearest.FeatureElement{
			Dim:   k,
			Value: float32(x),
		}
		i++
	}
	return ret, nil
}

type ID int64

func minInt(x, y int) int {
	if x < y {
		return x
	}
	return y
}

func maxInt(x, y int) int {
	if x < y {
		return y
	}
	return x
}

func maxFloat32(x, y float32) float32 {
	if x < y {
		return y
	}
	return x
}

func isInf32(x float32) bool {
	return math.IsInf(float64(x), 0)
}

var inf32 = float32(math.Inf(1))
