"""
HDBSCAN clustering for note embeddings.

Pure math — no database access, no auth, no permission logic.
Reads embeddings as JSON from stdin, writes cluster assignments to stdout.

Input format (JSON):
{
    "embeddings": [
        {"namespace": "home", "slug": "notes/foo", "vector": [0.1, 0.2, ...]},
        ...
    ],
    "min_cluster_size": 5  // optional, default 5
}

Output format (JSON):
{
    "clusters": [
        {"namespace": "home", "slug": "notes/foo", "cluster_id": 0},
        {"namespace": "home", "slug": "notes/bar", "cluster_id": 0},
        {"namespace": "shared", "slug": "GUIDELINES", "cluster_id": 1},
        {"namespace": "work", "slug": "notes/baz", "cluster_id": -1},
        ...
    ],
    "labels": {
        "0": "deployment, infrastructure, ansible",
        "1": "guidelines, communication, agents"
    },
    "stats": {
        "total_notes": 42,
        "clustered": 35,
        "noise": 7,
        "num_clusters": 4
    }
}

cluster_id -1 means noise (unclustered).
"""

import sys
import json
import numpy as np
from sklearn.cluster import HDBSCAN
from collections import Counter

def generate_cluster_labels(embeddings_data, assignments, top_n=3):
    """Generate simple labels for each cluster from note slugs/namespaces.

    Extracts the most common meaningful words from slug paths
    of notes in each cluster. Not perfect, but useful without
    an LLM call.
    """
    labels = {}
    cluster_ids = set(assignments)
    cluster_ids.discard(-1)

    for cid in sorted(cluster_ids):
        # Collect slug segments from all notes in this cluster
        words = []
        for i, assignment in enumerate(assignments):
            if assignment == cid:
                slug = embeddings_data[i]["slug"]
                # Split slug into path segments, then split on hyphens
                for segment in slug.split("/"):
                    for word in segment.split("-"):
                        word = word.lower().strip()
                        # Skip very short words and common prefixes
                        if len(word) > 2 and word not in ("notes", "tasks", "the", "and", "for"):
                            words.append(word)

        # Most common words become the label
        most_common = [w for w, _ in Counter(words).most_common(top_n)]
        labels[str(cid)] = ", ".join(most_common) if most_common else f"cluster-{cid}"

    return labels


def main():
    # Read input from stdin
    raw = sys.stdin.read()
    if not raw.strip():
        json.dump({"error": "empty input"}, sys.stdout)
        sys.exit(1)

    data = json.loads(raw)
    embeddings_data = data.get("embeddings", [])
    min_cluster_size = data.get("min_cluster_size", 5)

    # Need at least min_cluster_size notes to form any cluster
    if len(embeddings_data) < min_cluster_size:
        # Return everything as noise — not enough data to cluster
        clusters = [
            {
                "namespace": e["namespace"],
                "slug": e["slug"],
                "cluster_id": -1
            }
            for e in embeddings_data
        ]
        json.dump({
            "clusters": clusters,
            "labels": {},
            "stats": {
                "total_notes": len(embeddings_data),
                "clustered": 0,
                "noise": len(embeddings_data),
                "num_clusters": 0
            }
        }, sys.stdout)
        return

    # Build the embedding matrix
    vectors = np.array([e["vector"] for e in embeddings_data], dtype=np.float32)

    # Run HDBSCAN
    # metric='euclidean' works well with normalized embeddings (OpenAI embeddings are normalized)
    # min_samples defaults to min_cluster_size which is fine for most cases
    clusterer = HDBSCAN(
        min_cluster_size=min_cluster_size,
        metric="euclidean",
        store_centers="centroid"
    )
    assignments = clusterer.fit_predict(vectors)

    # Build output
    clusters = []
    for i, assignment in enumerate(assignments):
        clusters.append({
            "namespace": embeddings_data[i]["namespace"],
            "slug": embeddings_data[i]["slug"],
            "cluster_id": int(assignment)
        })

    # Generate labels from slug paths
    labels = generate_cluster_labels(embeddings_data, assignments)

    # Stats
    num_noise = int(np.sum(assignments == -1))
    num_clustered = len(assignments) - num_noise
    num_clusters = len(set(assignments) - {-1})

    json.dump({
        "clusters": clusters,
        "labels": labels,
        "stats": {
            "total_notes": len(embeddings_data),
            "clustered": num_clustered,
            "noise": num_noise,
            "num_clusters": num_clusters
        }
    }, sys.stdout)


if __name__ == "__main__":
    main()
