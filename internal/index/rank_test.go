package index

import "testing"

func TestCosineIdentical(t *testing.T) {
	v := []float32{1, 2, 3}
	if got := Cosine(v, v); got < 0.999 || got > 1.001 {
		t.Errorf("Cosine(v, v) = %v, want ~1", got)
	}
}

func TestCosineOrthogonal(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	if got := Cosine(a, b); got < -0.001 || got > 0.001 {
		t.Errorf("Cosine(orthogonal) = %v, want ~0", got)
	}
}

func TestCosineDimensionMismatch(t *testing.T) {
	// Comparing embeddings from two different models must never panic — it
	// is a configuration error the caller should catch, not the vector math.
	if got := Cosine([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Errorf("Cosine(mismatched dims) = %v, want 0", got)
	}
}

func TestFuseRRFPromotesAgreement(t *testing.T) {
	// id 5 ranks second in both lists; id 1 tops the vector list but is
	// absent from keyword results. Reciprocal rank fusion should still place
	// id 5 at or above id 1, since it is corroborated by both signals.
	vector := []int64{1, 5, 9}
	keyword := []int64{5, 7}
	fused := FuseRRF(vector, keyword)
	if len(fused) != 4 {
		t.Fatalf("got %d fused ids, want 4", len(fused))
	}
	pos := map[int64]int{}
	for i, id := range fused {
		pos[id] = i
	}
	if pos[5] > pos[1] {
		t.Errorf("expected id 5 (in both rankings) to rank at or above id 1 (vector-only); got order %v", fused)
	}
}

func TestFuseRRFDeterministicOnTie(t *testing.T) {
	a := FuseRRF([]int64{3, 1, 2})
	b := FuseRRF([]int64{3, 1, 2})
	if len(a) != len(b) {
		t.Fatalf("length mismatch: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("non-deterministic order at %d: %v vs %v", i, a, b)
		}
	}
}
