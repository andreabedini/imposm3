package cache

import (
	"bytes"
	"encoding/binary"
	"github.com/jmhodges/levigo"
	"goposm/element"
	"log"
	"runtime"
	"sync"
)

type bunchCache map[int64]RefBunch

type BunchRefCache struct {
	Cache
	cache     bunchCache
	write     chan bunchCache
	add       chan idRef
	mu        sync.Mutex
	waitAdd   *sync.WaitGroup
	waitWrite *sync.WaitGroup
}

type IdRef struct {
	id   int64
	refs []int64
}

var bunchCaches chan bunchCache

func init() {
	bunchCaches = make(chan bunchCache, 1)
}

type RefBunch map[int64][]int64

func NewBunchRefCache(path string, opts *CacheOptions) (*BunchRefCache, error) {
	index := BunchRefCache{}
	index.options = opts
	err := index.open(path)
	if err != nil {
		return nil, err
	}
	index.write = make(chan bunchCache, 2)
	index.cache = make(bunchCache, cacheSize)
	index.add = make(chan idRef, 1024)

	index.waitWrite = &sync.WaitGroup{}
	index.waitAdd = &sync.WaitGroup{}
	index.waitWrite.Add(1)
	index.waitAdd.Add(1)

	go index.writer()
	go index.dispatch()
	return &index, nil
}

func (index *BunchRefCache) writer() {
	for cache := range index.write {
		if err := index.writeRefs(cache); err != nil {
			log.Println("error while writing ref index", err)
		}
	}
	index.waitWrite.Done()
}

func (index *BunchRefCache) Close() {
	close(index.add)
	index.waitAdd.Wait()
	close(index.write)
	index.waitWrite.Wait()
	index.Cache.Close()
}

func (index *BunchRefCache) dispatch() {
	for idRef := range index.add {
		index.addToCache(idRef.id, idRef.ref)
		if len(index.cache) >= cacheSize {
			index.write <- index.cache
			select {
			case index.cache = <-bunchCaches:
			default:
				index.cache = make(map[int64]RefBunch, cacheSize)
			}
		}
	}
	if len(index.cache) > 0 {
		index.write <- index.cache
		index.cache = nil
	}
	index.waitAdd.Done()
}

func (index *BunchRefCache) AddFromWay(way *element.Way) {
	for _, node := range way.Nodes {
		index.add <- idRef{node.Id, way.Id}
	}
}

func (index *BunchRefCache) getBunchId(id int64) int64 {
	return id / 64
}

func (index *BunchRefCache) addToCache(id, ref int64) {
	bunchId := index.getBunchId(id)

	bunch, ok := index.cache[bunchId]
	if !ok {
		bunch = RefBunch{}
	}

	refs, ok := bunch[id]
	if !ok {
		refs = make([]int64, 0, 1)
	}
	refs = insertRefs(refs, ref)
	bunch[id] = refs
	index.cache[bunchId] = bunch
}

type loadBunchItem struct {
	bunchId int64
	bunch   RefBunch
}

type writeBunchItem struct {
	bunchIdBuf []byte
	data       []byte
}

func (index *BunchRefCache) writeRefs(idRefs map[int64]RefBunch) error {
	batch := levigo.NewWriteBatch()
	defer batch.Close()

	wg := sync.WaitGroup{}
	putc := make(chan writeBunchItem)
	loadc := make(chan loadBunchItem)

	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			for item := range loadc {
				keyBuf := idToKeyBuf(item.bunchId)
				putc <- writeBunchItem{
					keyBuf,
					index.loadMergeMarshal(keyBuf, item.bunch),
				}
			}
			wg.Done()
		}()
	}

	go func() {
		for bunchId, bunch := range idRefs {
			loadc <- loadBunchItem{bunchId, bunch}
		}
		close(loadc)
		wg.Wait()
		close(putc)
	}()

	for item := range putc {
		batch.Put(item.bunchIdBuf, item.data)
	}

	go func() {
		for k, _ := range idRefs {
			delete(idRefs, k)
		}
		select {
		case bunchCaches <- idRefs:
		}
	}()
	return index.db.Write(index.wo, batch)

}
func (index *BunchRefCache) loadMergeMarshal(keyBuf []byte, newBunch RefBunch) []byte {
	data, err := index.db.Get(index.ro, keyBuf)
	if err != nil {
		panic(err)
	}

	bunch := make(RefBunch)

	if data != nil {
		for _, idRef := range UnmarshalBunch(data) {
			bunch[idRef.id] = idRef.refs
		}
	}

	if bunch == nil {
		bunch = newBunch
	} else {
		for id, newRefs := range newBunch {
			refs, ok := bunch[id]
			if !ok {
				bunch[id] = newRefs
			} else {
				for _, ref := range newRefs {
					refs = insertRefs(refs, ref)

				}
				// sort.Sort(Refs(refs))
				bunch[id] = refs
			}
		}
	}

	bunchList := make([]IdRef, len(bunch))
	for id, refs := range bunch {
		bunchList = append(bunchList, IdRef{id, refs})
	}
	data = MarshalBunch(bunchList)
	return data
}

func (index *BunchRefCache) Get(id int64) []int64 {
	keyBuf := idToKeyBuf(index.getBunchId(id))

	data, err := index.db.Get(index.ro, keyBuf)
	if err != nil {
		panic(err)
	}

	if data != nil {
		for _, idRef := range UnmarshalBunch(data) {
			if idRef.id == id {
				return idRef.refs
			}
		}
	}
	return nil
}

func MarshalBunch(idRefs []IdRef) []byte {
	buf := make([]byte, len(idRefs)*(4+1+6)+binary.MaxVarintLen64)

	lastRef := int64(0)
	lastId := int64(0)
	nextPos := 0

	nextPos += binary.PutUvarint(buf[nextPos:], uint64(len(idRefs)))

	for _, idRef := range idRefs {
		if len(buf)-nextPos < binary.MaxVarintLen64 {
			tmp := make([]byte, len(buf)*2)
			copy(tmp, buf)
			buf = tmp
		}
		nextPos += binary.PutVarint(buf[nextPos:], idRef.id-lastId)
		lastId = idRef.id
	}
	for _, idRef := range idRefs {
		if len(buf)-nextPos < binary.MaxVarintLen64 {
			tmp := make([]byte, len(buf)*2)
			copy(tmp, buf)
			buf = tmp
		}
		nextPos += binary.PutUvarint(buf[nextPos:], uint64(len(idRef.refs)))
	}
	for _, idRef := range idRefs {
		for _, ref := range idRef.refs {
			if len(buf)-nextPos < binary.MaxVarintLen64 {
				tmp := make([]byte, len(buf)*2)
				copy(tmp, buf)
				buf = tmp
			}
			nextPos += binary.PutVarint(buf[nextPos:], ref-lastRef)
			lastRef = ref
		}
	}
	return buf[:nextPos]
}

func UnmarshalBunch(buf []byte) []IdRef {

	r := bytes.NewBuffer(buf)
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil
	}

	idRefs := make([]IdRef, n)

	last := int64(0)
	for i := 0; uint64(i) < n; i++ {
		idRefs[i].id, err = binary.ReadVarint(r)
		if err != nil {
			panic(err)
		}
		idRefs[i].id += last
		last = idRefs[i].id
	}
	var numRefs uint64
	for i := 0; uint64(i) < n; i++ {
		numRefs, err = binary.ReadUvarint(r)
		if err != nil {
			panic(err)
		}
		idRefs[i].refs = make([]int64, numRefs)
	}
	last = 0
	for idIdx := 0; uint64(idIdx) < n; idIdx++ {
		for refIdx := 0; refIdx < len(idRefs[idIdx].refs); refIdx++ {
			idRefs[idIdx].refs[refIdx], err = binary.ReadVarint(r)
			if err != nil {
				panic(err)
			}
			idRefs[idIdx].refs[refIdx] += last
			last = idRefs[idIdx].refs[refIdx]
		}
	}
	return idRefs
}
