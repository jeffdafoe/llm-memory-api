// Graph composable — D3 force-directed visualization of note clusters + slug references.
// Notes are colored by HDBSCAN cluster membership, connected by slug reference edges.

import { ref } from 'vue';
import { forceSimulation, forceLink, forceManyBody, forceCenter, forceCollide, forceX, forceY } from 'd3-force';
import { select } from 'd3-selection';
import { zoom } from 'd3-zoom';
import { drag } from 'd3-drag';

// Cluster color palette
const CLUSTER_COLORS = [
    '#3b82f6', '#22c55e', '#f59e0b', '#ef4444', '#8b5cf6',
    '#06b6d4', '#ec4899', '#f97316', '#14b8a6', '#6366f1',
    '#84cc16', '#f43f5e',
];

const NOISE_COLOR = '#3f3f46';
const EDGE_COLOR = '#52525b'; // zinc-600

export function useGraph({ api, showToast }) {
    const graphMode = ref(false);
    const graphData = ref(null);       // { clusters, references }
    const graphLoading = ref(false);
    const graphNamespaceFilter = ref('');
    const graphClusterFilter = ref('');
    const graphHideNoise = ref(false);
    const graphSelectedNode = ref(null);
    let onNodeClick = null;

    let simulation = null;
    let svgElement = null;
    let currentZoom = null;

    function clusterColor(clusterId) {
        if (clusterId === -1) return NOISE_COLOR;
        return CLUSTER_COLORS[clusterId % CLUSTER_COLORS.length];
    }

    // Load both clusters and slug references
    async function loadGraphData() {
        graphLoading.value = true;
        try {
            const [clusterData, refData] = await Promise.all([
                api('/admin/notes/clusters', {}),
                api('/admin/notes/references', {})
            ]);
            graphData.value = {
                clusters: clusterData.clusters || [],
                references: refData.references || []
            };
        } catch (err) {
            showToast('Failed to load graph: ' + err.message, 'error');
        } finally {
            graphLoading.value = false;
        }
    }

    // Build filtered nodes and edges
    function getFilteredData() {
        if (!graphData.value) return { nodes: [], edges: [] };

        var clusters = graphData.value.clusters;

        if (graphHideNoise.value) {
            clusters = clusters.filter(c => c.cluster_id !== -1);
        }

        if (graphClusterFilter.value !== '') {
            var cid = parseInt(graphClusterFilter.value, 10);
            clusters = clusters.filter(c => c.cluster_id === cid);
        }

        // Build node list and lookup
        var nodeMap = {};
        var nodes = [];
        for (var cluster of clusters) {
            for (var note of cluster.notes) {
                if (graphNamespaceFilter.value && note.namespace !== graphNamespaceFilter.value) {
                    continue;
                }
                var id = note.namespace + '/' + note.slug;
                if (!nodeMap[id]) {
                    var node = {
                        id: id,
                        namespace: note.namespace,
                        slug: note.slug,
                        cluster_id: cluster.cluster_id,
                        cluster_label: cluster.label
                    };
                    nodeMap[id] = node;
                    nodes.push(node);
                }
            }
        }

        // Filter references to only include edges where both endpoints are visible
        var edges = [];
        for (var ref of graphData.value.references) {
            var sourceKey = ref.source_namespace + '/' + ref.source_slug;
            var targetKey = ref.target_namespace + '/' + ref.target_slug;
            if (nodeMap[sourceKey] && nodeMap[targetKey]) {
                edges.push({ source: sourceKey, target: targetKey });
            }
        }

        return { nodes, edges };
    }

    function renderGraph(containerId) {
        var container = document.getElementById(containerId);
        if (!container) return;

        var data = getFilteredData();
        var nodes = data.nodes;
        var edges = data.edges;

        if (simulation) {
            simulation.stop();
            simulation = null;
        }
        select(container).selectAll('*').remove();

        if (nodes.length === 0) {
            select(container).append('div')
                .style('display', 'flex')
                .style('align-items', 'center')
                .style('justify-content', 'center')
                .style('height', '100%')
                .style('color', 'var(--text-muted)')
                .style('font-size', '14px')
                .text(graphData.value ? 'No notes match the current filters.' : 'Loading...');
            return;
        }

        var width = container.clientWidth;
        var height = container.clientHeight;

        // Compute cluster centers arranged in a circle
        var clusterIds = [...new Set(nodes.map(n => n.cluster_id))].sort((a, b) => a - b);
        var clusterCenters = {};
        var radius = Math.min(width, height) * 0.3;
        var nonNoiseCount = clusterIds.filter(id => id !== -1).length;
        var angleIdx = 0;
        clusterIds.forEach(function(cid) {
            if (cid === -1) {
                clusterCenters[cid] = { x: width / 2, y: height / 2 };
            } else {
                var angle = (2 * Math.PI * angleIdx) / Math.max(nonNoiseCount, 1);
                clusterCenters[cid] = {
                    x: width / 2 + radius * Math.cos(angle),
                    y: height / 2 + radius * Math.sin(angle)
                };
                angleIdx++;
            }
        });

        // Count connections per node for sizing
        var connectionCount = {};
        for (var e of edges) {
            connectionCount[e.source] = (connectionCount[e.source] || 0) + 1;
            connectionCount[e.target] = (connectionCount[e.target] || 0) + 1;
        }

        // Build node objects with initial positions near cluster centers
        var nodeObjects = nodes.map(function(n) {
            return {
                ...n,
                x: clusterCenters[n.cluster_id].x + (Math.random() - 0.5) * 60,
                y: clusterCenters[n.cluster_id].y + (Math.random() - 0.5) * 60
            };
        });

        // Build link objects
        var linkObjects = edges.map(function(e) {
            return { source: e.source, target: e.target };
        });

        // Create SVG
        var svg = select(container)
            .append('svg')
            .attr('width', width)
            .attr('height', height)
            .style('background', 'var(--bg-base)');

        svgElement = svg;

        // Zoom container
        var g = svg.append('g');
        currentZoom = zoom()
            .scaleExtent([0.1, 8])
            .on('zoom', function(event) {
                g.attr('transform', event.transform);
            });
        svg.call(currentZoom);

        // Arrow markers for edges
        var defs = svg.append('defs');
        defs.append('marker')
            .attr('id', 'arrow-ref')
            .attr('viewBox', '0 -5 10 10')
            .attr('refX', 20)
            .attr('refY', 0)
            .attr('markerWidth', 5)
            .attr('markerHeight', 5)
            .attr('orient', 'auto')
            .append('path')
            .attr('d', 'M0,-4L10,0L0,4')
            .attr('fill', EDGE_COLOR);

        // Draw cluster labels
        var labelData = clusterIds.filter(function(id) { return id !== -1; }).map(function(id) {
            var cluster = graphData.value.clusters.find(function(c) { return c.cluster_id === id; });
            return {
                id: id,
                label: cluster ? cluster.label : 'cluster ' + id,
                x: clusterCenters[id].x,
                y: clusterCenters[id].y
            };
        });

        g.append('g')
            .selectAll('text')
            .data(labelData)
            .join('text')
            .text(function(d) { return d.label; })
            .attr('x', function(d) { return d.x; })
            .attr('y', function(d) { return d.y - 60; })
            .attr('text-anchor', 'middle')
            .attr('font-size', 11)
            .attr('font-weight', 500)
            .attr('fill', function(d) { return clusterColor(d.id); })
            .style('opacity', 0.6)
            .style('pointer-events', 'none');

        // Draw edges (behind nodes)
        var link = g.append('g')
            .selectAll('line')
            .data(linkObjects)
            .join('line')
            .attr('stroke', EDGE_COLOR)
            .attr('stroke-width', 1)
            .attr('stroke-opacity', 0.4)
            .attr('marker-end', 'url(#arrow-ref)');

        // Draw nodes
        var node = g.append('g')
            .selectAll('g')
            .data(nodeObjects)
            .join('g')
            .style('cursor', 'pointer')
            .on('click', function(event, d) {
                event.stopPropagation();
                if (onNodeClick) {
                    onNodeClick(d);
                } else {
                    graphSelectedNode.value = d;
                }
            });

        // Node circles — sized by connection count
        node.append('circle')
            .attr('r', function(d) {
                var conns = connectionCount[d.id] || 0;
                if (d.cluster_id === -1 && conns === 0) return 4;
                return Math.min(16, 5 + conns * 1.5);
            })
            .attr('fill', function(d) { return clusterColor(d.cluster_id); })
            .attr('stroke', 'var(--bg-surface)')
            .attr('stroke-width', 1.5)
            .attr('opacity', function(d) { return d.cluster_id === -1 ? 0.5 : 0.85; });

        // Node labels
        node.append('text')
            .text(function(d) {
                var parts = d.slug.split('/');
                return parts[parts.length - 1];
            })
            .attr('font-size', 10)
            .attr('fill', 'var(--text-primary)')
            .attr('dx', function(d) {
                var conns = connectionCount[d.id] || 0;
                var r = (d.cluster_id === -1 && conns === 0) ? 4 : Math.min(16, 5 + conns * 1.5);
                return r + 4;
            })
            .attr('dy', 3)
            .style('pointer-events', 'none');

        // Namespace badge
        node.append('text')
            .text(function(d) { return d.namespace; })
            .attr('font-size', 8)
            .attr('fill', function(d) { return clusterColor(d.cluster_id); })
            .attr('dx', function(d) {
                var conns = connectionCount[d.id] || 0;
                var r = (d.cluster_id === -1 && conns === 0) ? 4 : Math.min(16, 5 + conns * 1.5);
                return r + 4;
            })
            .attr('dy', 14)
            .style('pointer-events', 'none')
            .style('opacity', 0.7);

        // Drag behavior
        var dragBehavior = drag()
            .on('start', function(event, d) {
                if (!event.active) simulation.alphaTarget(0.3).restart();
                d.fx = d.x;
                d.fy = d.y;
            })
            .on('drag', function(event, d) {
                d.fx = event.x;
                d.fy = event.y;
            })
            .on('end', function(event, d) {
                if (!event.active) simulation.alphaTarget(0);
                d.fx = null;
                d.fy = null;
            });

        node.call(dragBehavior);

        // Click background to deselect
        svg.on('click', function() {
            graphSelectedNode.value = null;
        });

        // Force simulation — links pull connected nodes together,
        // cluster forces group by topic
        simulation = forceSimulation(nodeObjects)
            .force('link', forceLink(linkObjects).id(function(d) { return d.id; }).distance(100).strength(0.3))
            .force('charge', forceManyBody().strength(-60))
            .force('center', forceCenter(width / 2, height / 2).strength(0.05))
            .force('collide', forceCollide(18))
            .force('x', forceX(function(d) { return clusterCenters[d.cluster_id].x; }).strength(function(d) { return d.cluster_id === -1 ? 0.02 : 0.1; }))
            .force('y', forceY(function(d) { return clusterCenters[d.cluster_id].y; }).strength(function(d) { return d.cluster_id === -1 ? 0.02 : 0.1; }))
            .on('tick', function() {
                link
                    .attr('x1', function(d) { return d.source.x; })
                    .attr('y1', function(d) { return d.source.y; })
                    .attr('x2', function(d) { return d.target.x; })
                    .attr('y2', function(d) { return d.target.y; });

                node.attr('transform', function(d) { return 'translate(' + d.x + ',' + d.y + ')'; });
            });
    }

    function destroyGraph() {
        if (simulation) {
            simulation.stop();
            simulation = null;
        }
        svgElement = null;
        currentZoom = null;
    }

    return {
        graphMode,
        graphData,
        graphLoading,
        graphNamespaceFilter,
        graphClusterFilter,
        graphHideNoise,
        graphSelectedNode,
        loadGraphData,
        renderGraph,
        destroyGraph,
        clusterColor,
        setOnNodeClick(fn) { onNodeClick = fn; },
    };
}
