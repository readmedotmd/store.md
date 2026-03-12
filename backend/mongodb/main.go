package mongodb

import (
	"context"
	"regexp"
	"time"

	storemd "github.com/readmedotmd/store.md"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const defaultTimeout = 30 * time.Second

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}

type document struct {
	ID    string `bson:"_id"`
	Value string `bson:"value"`
}

type StoreMongo struct {
	col *mongo.Collection
}

func New(col *mongo.Collection) *StoreMongo {
	return &StoreMongo{col: col}
}

// Clear removes all key-value pairs from the collection.
func (s *StoreMongo) Clear(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := s.col.DeleteMany(ctx, bson.M{})
	return err
}

func (s *StoreMongo) Get(ctx context.Context, key string) (string, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var doc document
	err := s.col.FindOne(ctx, bson.M{"_id": key}).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return "", storemd.NotFoundError
		}
		return "", err
	}
	return doc.Value, nil
}

func (s *StoreMongo) Set(ctx context.Context, key, value string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	opts := options.Replace().SetUpsert(true)
	_, err := s.col.ReplaceOne(
		ctx,
		bson.M{"_id": key},
		document{ID: key, Value: value},
		opts,
	)
	return err
}

func (s *StoreMongo) SetIfNotExists(ctx context.Context, key, value string) (bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := s.col.InsertOne(ctx, document{ID: key, Value: value})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *StoreMongo) Delete(ctx context.Context, key string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	result, err := s.col.DeleteOne(ctx, bson.M{"_id": key})
	if err != nil {
		return err
	}
	if result.DeletedCount == 0 {
		return storemd.NotFoundError
	}
	return nil
}

func (s *StoreMongo) List(ctx context.Context, args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	filter := bson.M{}

	if args.Prefix != "" {
		filter["_id"] = bson.M{"$regex": "^" + regexp.QuoteMeta(args.Prefix)}
	}

	if args.StartAfter != "" {
		if existing, ok := filter["_id"]; ok {
			filter["$and"] = bson.A{
				bson.M{"_id": existing},
				bson.M{"_id": bson.M{"$gt": args.StartAfter}},
			}
			delete(filter, "_id")
		} else {
			filter["_id"] = bson.M{"$gt": args.StartAfter}
		}
	}

	opts := options.Find().SetSort(bson.M{"_id": 1})

	if args.Limit > 0 {
		opts.SetLimit(int64(args.Limit))
	}

	cursor, err := s.col.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var results []storemd.KeyValuePair
	for cursor.Next(ctx) {
		var doc document
		if err := cursor.Decode(&doc); err != nil {
			return nil, err
		}
		results = append(results, storemd.KeyValuePair{Key: doc.ID, Value: doc.Value})
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}

	if results == nil {
		results = []storemd.KeyValuePair{}
	}

	return results, nil
}
