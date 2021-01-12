package kv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/testutil/assert"
	"github.com/prysmaticlabs/prysm/shared/testutil/require"
	logTest "github.com/sirupsen/logrus/hooks/test"
	bolt "go.etcd.io/bbolt"
)

func TestStore_CheckSlashableAttestation_DoubleVote(t *testing.T) {
	ctx := context.Background()
	numValidators := 1
	pubKeys := make([][48]byte, numValidators)
	validatorDB := setupDB(t, pubKeys)
	tests := []struct {
		name                string
		existingAttestation *ethpb.IndexedAttestation
		existingSigningRoot [32]byte
		incomingAttestation *ethpb.IndexedAttestation
		incomingSigningRoot [32]byte
		want                bool
	}{
		{
			name:                "different signing root at same target equals a double vote",
			existingAttestation: createAttestation(0, 1 /* target */),
			existingSigningRoot: [32]byte{1},
			incomingAttestation: createAttestation(0, 1 /* target */),
			incomingSigningRoot: [32]byte{2},
			want:                true,
		},
		{
			name:                "same signing root at same target is safe",
			existingAttestation: createAttestation(0, 1 /* target */),
			existingSigningRoot: [32]byte{1},
			incomingAttestation: createAttestation(0, 1 /* target */),
			incomingSigningRoot: [32]byte{1},
			want:                false,
		},
		{
			name:                "different signing root at different target is safe",
			existingAttestation: createAttestation(0, 1 /* target */),
			existingSigningRoot: [32]byte{1},
			incomingAttestation: createAttestation(0, 2 /* target */),
			incomingSigningRoot: [32]byte{2},
			want:                false,
		},
		{
			name:                "no data stored at target should not be considered a double vote",
			existingAttestation: createAttestation(0, 1 /* target */),
			existingSigningRoot: [32]byte{1},
			incomingAttestation: createAttestation(0, 2 /* target */),
			incomingSigningRoot: [32]byte{1},
			want:                false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatorDB.SaveAttestationForPubKey(
				ctx,
				pubKeys[0],
				tt.existingSigningRoot,
				tt.existingAttestation,
			)
			require.NoError(t, err)
			slashingKind, err := validatorDB.CheckSlashableAttestation(
				ctx,
				pubKeys[0],
				tt.incomingSigningRoot,
				tt.incomingAttestation,
			)
			if tt.want {
				require.NotNil(t, err)
				assert.Equal(t, DoubleVote, slashingKind)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestStore_CheckSlashableAttestation_SurroundVote_54kEpochs(t *testing.T) {
	ctx := context.Background()
	numValidators := 1
	numEpochs := uint64(54000)
	pubKeys := make([][48]byte, numValidators)
	validatorDB := setupDB(t, pubKeys)

	// Attest to every (source = epoch, target = epoch + 1) sequential pair
	// since genesis up to and including the weak subjectivity period epoch (54,000).
	err := validatorDB.update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(pubKeysBucket)
		pkBucket, err := bucket.CreateBucketIfNotExists(pubKeys[0][:])
		if err != nil {
			return err
		}
		sourceEpochsBucket, err := pkBucket.CreateBucketIfNotExists(attestationSourceEpochsBucket)
		if err != nil {
			return err
		}
		for epoch := uint64(1); epoch < numEpochs; epoch++ {
			att := createAttestation(epoch-1, epoch)
			sourceEpoch := bytesutil.Uint64ToBytesBigEndian(att.Data.Source.Epoch)
			targetEpoch := bytesutil.Uint64ToBytesBigEndian(att.Data.Target.Epoch)
			if err := sourceEpochsBucket.Put(sourceEpoch, targetEpoch); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)

	tests := []struct {
		name        string
		signingRoot [32]byte
		attestation *ethpb.IndexedAttestation
		want        SlashingKind
	}{
		{
			name:        "surround vote at half of the weak subjectivity period",
			signingRoot: [32]byte{},
			attestation: createAttestation(numEpochs/2, numEpochs),
			want:        SurroundingVote,
		},
		{
			name:        "spanning genesis to weak subjectivity period surround vote",
			signingRoot: [32]byte{},
			attestation: createAttestation(0, numEpochs),
			want:        SurroundingVote,
		},
		{
			name:        "simple surround vote at end of weak subjectivity period",
			signingRoot: [32]byte{},
			attestation: createAttestation(numEpochs-3, numEpochs),
			want:        SurroundingVote,
		},
		{
			name:        "non-slashable vote",
			signingRoot: [32]byte{},
			attestation: createAttestation(numEpochs, numEpochs+1),
			want:        NotSlashable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slashingKind, err := validatorDB.CheckSlashableAttestation(ctx, pubKeys[0], tt.signingRoot, tt.attestation)
			if tt.want != NotSlashable {
				require.NotNil(t, err)
			}
			assert.Equal(t, tt.want, slashingKind)
		})
	}
}

func TestSaveAttestationForPubKey_BatchWrites_FullCapacity(t *testing.T) {
	hook := logTest.NewGlobal()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	numValidators := attestationBatchCapacity
	pubKeys := make([][48]byte, numValidators)
	validatorDB := setupDB(t, pubKeys)

	// For each public key, we attempt to save an attestation with signing root.
	var wg sync.WaitGroup
	for i, pubKey := range pubKeys {
		wg.Add(1)
		go func(j int, pk [48]byte, w *sync.WaitGroup) {
			defer w.Done()
			var signingRoot [32]byte
			copy(signingRoot[:], fmt.Sprintf("%d", j))
			att := createAttestation(uint64(j), uint64(j)+1)
			err := validatorDB.SaveAttestationForPubKey(ctx, pk, signingRoot, att)
			require.NoError(t, err)
		}(i, pubKey, &wg)
	}
	wg.Wait()

	// We verify that we reached the max capacity of batched attestations
	// before we are required to force flush them to the DB.
	require.LogsContain(t, hook, "Reached max capacity of batched attestation records")
	require.LogsDoNotContain(t, hook, "Batched attestation records write interval reached")
	require.LogsContain(t, hook, "Successfully flushed batched attestations to DB")
	require.Equal(t, 0, len(validatorDB.batchedAttestations))

	// We then verify all the data we wanted to save is indeed saved to disk.
	err := validatorDB.view(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(pubKeysBucket)
		for i, pubKey := range pubKeys {
			var signingRoot [32]byte
			copy(signingRoot[:], fmt.Sprintf("%d", i))
			pkBucket := bucket.Bucket(pubKey[:])
			signingRootsBucket := pkBucket.Bucket(attestationSigningRootsBucket)
			sourceEpochsBucket := pkBucket.Bucket(attestationSourceEpochsBucket)

			source := bytesutil.Uint64ToBytesBigEndian(uint64(i))
			target := bytesutil.Uint64ToBytesBigEndian(uint64(i) + 1)
			savedSigningRoot := signingRootsBucket.Get(target)
			require.DeepEqual(t, signingRoot[:], savedSigningRoot)
			savedTarget := sourceEpochsBucket.Get(source)
			require.DeepEqual(t, signingRoot[:], savedSigningRoot)
			require.DeepEqual(t, target, savedTarget)
		}
		return nil
	})
	require.NoError(t, err)
}

func TestSaveAttestationForPubKey_BatchWrites_LowCapacity_TimerReached(t *testing.T) {
	hook := logTest.NewGlobal()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Number of validators equal to half the total capacity
	// of batch attestation processing. This will allow us to
	// test force flushing to the DB based on a timer instead
	// of the max capacity being reached.
	numValidators := attestationBatchCapacity / 2
	pubKeys := make([][48]byte, numValidators)
	validatorDB := setupDB(t, pubKeys)

	// For each public key, we attempt to save an attestation with signing root.
	var wg sync.WaitGroup
	for i, pubKey := range pubKeys {
		wg.Add(1)
		go func(j int, pk [48]byte, w *sync.WaitGroup) {
			defer w.Done()
			var signingRoot [32]byte
			copy(signingRoot[:], fmt.Sprintf("%d", j))
			att := createAttestation(uint64(j), uint64(j)+1)
			err := validatorDB.SaveAttestationForPubKey(ctx, pk, signingRoot, att)
			require.NoError(t, err)
		}(i, pubKey, &wg)
	}
	wg.Wait()

	// We verify that we reached a timer interval for force flushing records
	// before we are required to force flush them to the DB.
	require.LogsDoNotContain(t, hook, "Reached max capacity of batched attestation records")
	require.LogsContain(t, hook, "Batched attestation records write interval reached")
	require.LogsContain(t, hook, "Successfully flushed batched attestations to DB")
	require.Equal(t, 0, len(validatorDB.batchedAttestations))

	// We then verify all the data we wanted to save is indeed saved to disk.
	err := validatorDB.view(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(pubKeysBucket)
		for i, pubKey := range pubKeys {
			var signingRoot [32]byte
			copy(signingRoot[:], fmt.Sprintf("%d", i))
			pkBucket := bucket.Bucket(pubKey[:])
			signingRootsBucket := pkBucket.Bucket(attestationSigningRootsBucket)
			sourceEpochsBucket := pkBucket.Bucket(attestationSourceEpochsBucket)

			source := bytesutil.Uint64ToBytesBigEndian(uint64(i))
			target := bytesutil.Uint64ToBytesBigEndian(uint64(i) + 1)
			savedSigningRoot := signingRootsBucket.Get(target)
			require.DeepEqual(t, signingRoot[:], savedSigningRoot)
			savedTarget := sourceEpochsBucket.Get(source)
			require.DeepEqual(t, signingRoot[:], savedSigningRoot)
			require.DeepEqual(t, target, savedTarget)
		}
		return nil
	})
	require.NoError(t, err)
}

func BenchmarkStore_CheckSlashableAttestation_Surround_SafeAttestation_54kEpochs(b *testing.B) {
	numValidators := 1
	numEpochs := uint64(54000)
	pubKeys := make([][48]byte, numValidators)
	benchCheckSurroundVote(b, pubKeys, numEpochs, false /* surround */)
}

func BenchmarkStore_CheckSurroundVote_Surround_Slashable_54kEpochs(b *testing.B) {
	numValidators := 1
	numEpochs := uint64(54000)
	pubKeys := make([][48]byte, numValidators)
	benchCheckSurroundVote(b, pubKeys, numEpochs, true /* surround */)
}

func benchCheckSurroundVote(
	b *testing.B,
	pubKeys [][48]byte,
	numEpochs uint64,
	shouldSurround bool,
) {
	ctx := context.Background()
	validatorDB, err := NewKVStore(ctx, filepath.Join(os.TempDir(), "benchsurroundvote"), pubKeys)
	require.NoError(b, err, "Failed to instantiate DB")
	defer func() {
		require.NoError(b, validatorDB.Close(), "Failed to close database")
		require.NoError(b, validatorDB.ClearDB(), "Failed to clear database")
	}()
	// Every validator will have attested every (source, target) sequential pair
	// since genesis up to and including the weak subjectivity period epoch (54,000).
	err = validatorDB.update(func(tx *bolt.Tx) error {
		for _, pubKey := range pubKeys {
			bucket := tx.Bucket(pubKeysBucket)
			pkBucket, err := bucket.CreateBucketIfNotExists(pubKey[:])
			if err != nil {
				return err
			}
			sourceEpochsBucket, err := pkBucket.CreateBucketIfNotExists(attestationSourceEpochsBucket)
			if err != nil {
				return err
			}
			for epoch := uint64(1); epoch < numEpochs; epoch++ {
				att := createAttestation(epoch-1, epoch)
				sourceEpoch := bytesutil.Uint64ToBytesBigEndian(att.Data.Source.Epoch)
				targetEpoch := bytesutil.Uint64ToBytesBigEndian(att.Data.Target.Epoch)
				if err := sourceEpochsBucket.Put(sourceEpoch, targetEpoch); err != nil {
					return err
				}
			}
		}
		return nil
	})
	require.NoError(b, err)

	// Will surround many attestations.
	var surroundingVote *ethpb.IndexedAttestation
	if shouldSurround {
		surroundingVote = createAttestation(numEpochs/2, numEpochs)
	} else {
		surroundingVote = createAttestation(numEpochs+1, numEpochs+2)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, pubKey := range pubKeys {
			slashingKind, err := validatorDB.CheckSlashableAttestation(ctx, pubKey, [32]byte{}, surroundingVote)
			if shouldSurround {
				require.NotNil(b, err)
				assert.Equal(b, SurroundingVote, slashingKind)
			} else {
				require.NoError(b, err)
			}
		}
	}
}

func createAttestation(source, target uint64) *ethpb.IndexedAttestation {
	return &ethpb.IndexedAttestation{
		Data: &ethpb.AttestationData{
			Source: &ethpb.Checkpoint{
				Epoch: source,
			},
			Target: &ethpb.Checkpoint{
				Epoch: target,
			},
		},
	}
}