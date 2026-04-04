// Graph composable — D3 force-directed visualization of note clusters.
// Notes are grouped by HDBSCAN cluster membership. Nodes are colored by
// cluster, with same-cluster notes pulled together by force simulation.

import { ref, nextTick, watch } from 'vue';
import { forceSimulation, forceManyBody, forceCenter, forceCollide, forceX, forceY } from 'd3-force';
import { select } from 'd3-selection';
import { zoom, zoomIdentity } from 'd3-zoom';
import { drag } from 'd3-drag';

// Cluster color palette — distinct colors for visual separation
const CLUSTER_COLORS = [
    '#3b82f6', // blue
    '#22c55e', // green
    '#f59e0b', // amber
    '#ef4444', // red
    '#8b5cf6', // purple
    '#06b6d4', // cyan
    '#ec4899', // pink
    '#f97316', // orange
    '#14b8a6', // teal
    '#6366f1', // indigo
    '#84cc16', // lime
    '#f43f5e', // rose
];

const NOISE_COLOR = '#3f3f46'; // zinc-700 — unclustered notes

export function useGraph({ api, showToast }) {
    const graphMode = ref(false);
    const graphData = ref(null);
    const graphLoading = ref(false);
    const graphNamespaceFilter = ref('');
    const graphClusterFilter = ref('');
    const graphHideNoise = ref(false);
    const graphSelectedNode = ref(null);
    let onNodeClick = null;

    let simulation = null;
    let svgElement = null;
    let currentZoom = null;

    // Color mapping for clusters
    function clusterColor(clusterId) {
        if (clusterId === -1) return NOISE_COLOR;
        return CLUSTER_COLORS[clusterId % CLUSTER_COLORS.length];
    }

    // Load cluster data from API
    async function loadGraphData() {
        graphLoading.value = true;
        try {
            const data = await api('/admin/notes/clusters', {});
            graphData.value = data;
        } catch (err) {
            showToast('Failed to load clusters: ' + err.message, 'error');
        } finally {
            graphLoading.value = false;
        }
    }

    // Get filtered data based on current filter settings
    function getFilteredData() {
        if (!graphData.value || !graphData.value.clusters) return { nodes: [], clusters: [] };

        let clusters = graphData.value.clusters;

        // Filter out noise if requested
        if (graphHideNoise.value) {
            clusters = clusters.filter(c => c.cluster_id !== -1);
        }

        // Filter by specific cluster
        if (graphClusterFilter.value !== '') {
            const cid = parseInt(graphClusterFilter.value, 10);
            clusters = clusters.filter(c => c.cluster_id === cid);
        }

        // Build flat node list from clusters
        let nodes = [];
        for (const cluster of clusters) {
            for (const note of cluster.notes) {
                // Filter by namespace
                if (graphNamespaceFilter.value && note.namespace !== graphNamespaceFilter.value) {
                    continue;
                }
                nodes.push({
                    id: note.namespace + '/' + note.slug,
                    namespace: note.namespace,
                    slug: note.slug,
                    cluster_id: cluster.cluster_id,
                    cluster_label: cluster.label
                });
            }
        }

        return { nodes, clusters };
    }

    // Initialize or update the D3 visualization
    function renderGraph(containerId) {
        const container = document.getElementById(containerId);
        if (!container) return;

        const { nodes } = getFilteredData();

        // Clean up previous
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
                .text(graphData.value ? 'No clusters match the current filters.' : 'Loading...');
            return;
        }

        const width = container.clientWidth;
        const height = container.clientHeight;

        // Compute cluster centers — arrange clusters in a circle around the center
        const clusterIds = [...new Set(nodes.map(n => n.cluster_id))].sort((a, b) => a - b);
        const clusterCenters = {};
        const radius = Math.min(width, height) * 0.3;
        clusterIds.forEach((cid, i) => {
            if (cid === -1) {
                // Noise nodes scatter near the edges
                clusterCenters[cid] = { x: width / 2, y: height / 2 };
            } else {
                const angle = (2 * Math.PI * i) / Math.max(clusterIds.length - (clusterIds.includes(-1) ? 1 : 0), 1);
                clusterCenters[cid] = {
                    x: width / 2 + radius * Math.cos(angle),
                    y: height / 2 + radius * Math.sin(angle)
                };
            }
        });

        // Build node objects
        const nodeObjects = nodes.map(n => ({
            ...n,
            x: clusterCenters[n.cluster_id].x + (Math.random() - 0.5) * 60,
            y: clusterCenters[n.cluster_id].y + (Math.random() - 0.5) * 60
        }));

        // Create SVG
        const svg = select(container)
            .append('svg')
            .attr('width', width)
            .attr('height', height)
            .style('background', 'var(--bg-base)');

        svgElement = svg;

        // Zoom container
        const g = svg.append('g');

        currentZoom = zoom()
            .scaleExtent([0.1, 8])
            .on('zoom', (event) => {
                g.attr('transform', event.transform);
            });
        svg.call(currentZoom);

        // Draw cluster labels at cluster centers
        const labelData = clusterIds.filter(id => id !== -1).map(id => {
            var cluster = graphData.value.clusters.find(c => c.cluster_id === id);
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
            .text(d => d.label)
            .attr('x', d => d.x)
            .attr('y', d => d.y - 60)
            .attr('text-anchor', 'middle')
            .attr('font-size', 11)
            .attr('font-weight', 500)
            .attr('fill', d => clusterColor(d.id))
            .style('opacity', 0.6)
            .style('pointer-events', 'none');

        // Draw nodes
        const node = g.append('g')
            .selectAll('g')
            .data(nodeObjects)
            .join('g')
            .style('cursor', 'pointer')
            .on('click', (event, d) => {
                event.stopPropagation();
                if (onNodeClick) {
                    onNodeClick(d);
                } else {
                    graphSelectedNode.value = d;
                }
            });

        // Node circles
        node.append('circle')
            .attr('r', d => d.cluster_id === -1 ? 4 : 6)
            .attr('fill', d => clusterColor(d.cluster_id))
            .attr('stroke', 'var(--bg-surface)')
            .attr('stroke-width', 1.5)
            .attr('opacity', d => d.cluster_id === -1 ? 0.5 : 0.85);

        // Node labels — show just the last part of the slug
        node.append('text')
            .text(d => {
                var parts = d.slug.split('/');
                return parts[parts.length - 1];
            })
            .attr('font-size', 10)
            .attr('fill', 'var(--text-primary)')
            .attr('dx', d => (d.cluster_id === -1 ? 4 : 6) + 4)
            .attr('dy', 3)
            .style('pointer-events', 'none');

        // Namespace badge
        node.append('text')
            .text(d => d.namespace)
            .attr('font-size', 8)
            .attr('fill', d => clusterColor(d.cluster_id))
            .attr('dx', d => (d.cluster_id === -1 ? 4 : 6) + 4)
            .attr('dy', 14)
            .style('pointer-events', 'none')
            .style('opacity', 0.7);

        // Drag behavior
        const dragBehavior = drag()
            .on('start', (event, d) => {
                if (!event.active) simulation.alphaTarget(0.3).restart();
                d.fx = d.x;
                d.fy = d.y;
            })
            .on('drag', (event, d) => {
                d.fx = event.x;
                d.fy = event.y;
            })
            .on('end', (event, d) => {
                if (!event.active) simulation.alphaTarget(0);
                d.fx = null;
                d.fy = null;
            });

        node.call(dragBehavior);

        // Click background to deselect
        svg.on('click', () => {
            graphSelectedNode.value = null;
        });

        // Force simulation — no links, just clustering forces
        simulation = forceSimulation(nodeObjects)
            .force('charge', forceManyBody().strength(-40))
            .force('center', forceCenter(width / 2, height / 2).strength(0.05))
            .force('collide', forceCollide(16))
            // Pull nodes toward their cluster center
            .force('x', forceX(d => clusterCenters[d.cluster_id].x).strength(d => d.cluster_id === -1 ? 0.02 : 0.15))
            .force('y', forceY(d => clusterCenters[d.cluster_id].y).strength(d => d.cluster_id === -1 ? 0.02 : 0.15))
            .on('tick', () => {
                node.attr('transform', d => `translate(${d.x},${d.y})`);
            });
    }

    // Clean up simulation when leaving graph mode
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
