package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoStore keeps jobs in the "jobs" collection. A unique index on request_id makes
// idempotent admission atomic: of two concurrent inserts with the same request_id, the
// database accepts exactly one.
type MongoStore struct {
	client *mongo.Client
	jobs   *mongo.Collection
}

// NewMongoStore connects, pings and ensures indexes.
func NewMongoStore(ctx context.Context, uri, database string) (*MongoStore, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", uri, err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("ping %s: %w", uri, err)
	}
	coll := client.Database(database).Collection("jobs")
	_, err = coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "request_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("uniq_request_id")},
		{Keys: bson.D{{Key: "timestamps.submitted_at", Value: -1}}, Options: options.Index().SetName("submitted_desc")},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "workload_id", Value: 1}}, Options: options.Index().SetName("status_workload")},
	})
	if err != nil {
		return nil, fmt.Errorf("create indexes: %w", err)
	}
	return &MongoStore{client: client, jobs: coll}, nil
}

func (s *MongoStore) Close(ctx context.Context) error { return s.client.Disconnect(ctx) }

func (s *MongoStore) Insert(ctx context.Context, j *Job) error {
	_, err := s.jobs.InsertOne(ctx, j)
	if mongo.IsDuplicateKeyError(err) {
		return ErrDuplicateRequest
	}
	return err
}

func (s *MongoStore) findOne(ctx context.Context, filter bson.M) (*Job, error) {
	var j Job
	err := s.jobs.FindOne(ctx, filter).Decode(&j)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *MongoStore) Get(ctx context.Context, id string) (*Job, error) {
	return s.findOne(ctx, bson.M{"_id": id})
}

func (s *MongoStore) GetByRequestID(ctx context.Context, rq string) (*Job, error) {
	return s.findOne(ctx, bson.M{"request_id": rq})
}

func (s *MongoStore) Replace(ctx context.Context, j *Job) error {
	res, err := s.jobs.ReplaceOne(ctx, bson.M{"_id": j.ID}, j)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) List(ctx context.Context, f ListFilter) ([]*Job, int, error) {
	filter := bson.M{}
	if f.Status != "" {
		filter["status"] = f.Status
	}
	if f.WorkloadID != "" {
		filter["workload_id"] = f.WorkloadID
	}
	total, err := s.jobs.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "timestamps.submitted_at", Value: -1}}).
		SetSkip(int64((f.Page - 1) * f.Limit)).
		SetLimit(int64(f.Limit))
	cur, err := s.jobs.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, err
	}
	var out []*Job
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, int(total), nil
}

func (s *MongoStore) ListActive(ctx context.Context) ([]*Job, error) {
	cur, err := s.jobs.Find(ctx, bson.M{"status": bson.M{"$in": []v1.JobState{v1.StatePending, v1.StateRunning}}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var out []*Job
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
