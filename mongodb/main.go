package mongodb

import (
	"context"
	"regexp"

	storemd "github.com/readmedotmd/store.md"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

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

func (s *StoreMongo) Get(key string) (string, error) {
	var doc document
	err := s.col.FindOne(context.Background(), bson.M{"_id": key}).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return "", storemd.NotFoundError
		}
		return "", err
	}
	return doc.Value, nil
}

func (s *StoreMongo) Set(key, value string) error {
	opts := options.Replace().SetUpsert(true)
	_, err := s.col.ReplaceOne(
		context.Background(),
		bson.M{"_id": key},
		document{ID: key, Value: value},
		opts,
	)
	return err
}

func (s *StoreMongo) Delete(key string) error {
	result, err := s.col.DeleteOne(context.Background(), bson.M{"_id": key})
	if err != nil {
		return err
	}
	if result.DeletedCount == 0 {
		return storemd.NotFoundError
	}
	return nil
}

func (s *StoreMongo) List(args storemd.ListArgs) ([]storemd.KeyValuePair, error) {
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

	cursor, err := s.col.Find(context.Background(), filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	var results []storemd.KeyValuePair
	for cursor.Next(context.Background()) {
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
