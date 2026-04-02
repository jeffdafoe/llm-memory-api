// Graph composable — D3 force-directed visualization of note relations.
// Renders in the notes view when graph mode is toggled on.

import { ref, nextTick, watch } from 'vue';
import { forceSimulation, forceLink, forceManyBody, forceCenter, forceCollide } from 'd3-force';
import { select } from 'd3-selection';
import { zoom, zoomIdentity } from 'd3-zoom';
import { drag } from 'd3-drag';

// Namespace color palette — distinct colors for visual separation
const NS_COLORS = [
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
];

// Line styles per relation type
const RELATION_STYLES = {
    'depends-on':  { dash: null,    color: '#ef4444' },  // solid red
    'references':  { dash: '6,3',   color: '#71717a' },  // dashed gray
    'supersedes':  { dash: '2,4',   color: '#f59e0b' },  // dotted amber
    'led-to':      { dash: null,    color: '#22c55e' },  // solid green
    'related':     { dash: '4,4',   color: '#3b82f6' },  // dashed blue
    'subtask-of':  { dash: null,    color: '#8b5cf6' },  // solid purple
};

export function useGraph({ api, showToast }) {
    const graphMode = ref(false);
    const graphData = ref(null);
    const graphLoading = ref(false);
    const graphNamespaceFilter = ref('');
    const graphTypeFilter = ref('');
    const graphShowAuto = ref(true);
    const graphSelectedNode = ref(null);

    let simulation = null;
    let svgElement = null;
    let currentZoom = null;

    // Color mapping for namespaces
    const nsColorMap = {};
    let nsColorIdx = 0;
    function nsColor(ns) {
        if (!nsColorMap[ns]) {
            nsColorMap[ns] = NS_COLORS[nsColorIdx % NS_COLORS.length];
            nsColorIdx++;
        }
        return nsColorMap[ns];
    }

    // Load graph data from API
    async function loadGraphData(namespace) {
        graphLoading.value = true;
        try {
            const body = namespace ? { namespace } : {};
            const data = await api('/admin/notes/graph-all', body);
            graphData.value = data;
        } catch (err) {
            showToast('Failed to load graph: ' + err.message, 'error');
        } finally {
            graphLoading.value = false;
        }
    }

    // Get filtered data based on current filter settings
    function getFilteredData() {
        if (!graphData.value) return { nodes: [], edges: [] };
        let { nodes, edges } = graphData.value;

        // Filter by namespace
        if (graphNamespaceFilter.value) {
            const ns = graphNamespaceFilter.value;
            edges = edges.filter(e => {
                const sNs = e.source.split ? e.source.split('/')[0] : (e.source.namespace || '');
                const tNs = e.target.split ? e.target.split('/')[0] : (e.target.namespace || '');
                return sNs === ns || tNs === ns;
            });
        }

        // Filter by relation type
        if (graphTypeFilter.value) {
            edges = edges.filter(e => e.type === graphTypeFilter.value);
        }

        // Filter auto-extracted
        if (!graphShowAuto.value) {
            edges = edges.filter(e => !e.auto_extracted);
        }

        // Rebuild node set from remaining edges
        const nodeKeys = new Set();
        for (const e of edges) {
            const sKey = e.source.split ? e.source : (e.source.namespace + '/' + e.source.slug);
            const tKey = e.target.split ? e.target : (e.target.namespace + '/' + e.target.slug);
            nodeKeys.add(sKey);
            nodeKeys.add(tKey);
        }
        nodes = nodes.filter(n => nodeKeys.has(n.namespace + '/' + n.slug));

        return { nodes, edges };
    }

    // Initialize or update the D3 visualization
    function renderGraph(containerId) {
        const container = document.getElementById(containerId);
        if (!container) return;

        const { nodes, edges } = getFilteredData();

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
                .text(graphData.value ? 'No relations match the current filters.' : 'Loading...');
            return;
        }

        const width = container.clientWidth;
        const height = container.clientHeight;

        // Build node objects with id = namespace/slug
        const nodeMap = {};
        const nodeObjects = nodes.map(n => {
            const id = n.namespace + '/' + n.slug;
            const obj = { id, namespace: n.namespace, slug: n.slug, root: n.root || false };
            nodeMap[id] = obj;
            return obj;
        });

        // Count connections per node for sizing
        const connectionCount = {};
        for (const e of edges) {
            const sKey = typeof e.source === 'string' ? e.source : e.source.id;
            const tKey = typeof e.target === 'string' ? e.target : e.target.id;
            connectionCount[sKey] = (connectionCount[sKey] || 0) + 1;
            connectionCount[tKey] = (connectionCount[tKey] || 0) + 1;
        }

        // Build link objects
        const linkObjects = edges.map(e => ({
            source: typeof e.source === 'string' ? e.source : e.source.id,
            target: typeof e.target === 'string' ? e.target : e.target.id,
            type: e.type,
            auto_extracted: e.auto_extracted,
            id: e.id
        }));

        // Create SVG
        const svg = select(container)
            .append('svg')
            .attr('width', width)
            .attr('height', height)
            .style('background', 'var(--bg-base)');

        svgElement = svg;

        // Arrow markers for each relation type
        const defs = svg.append('defs');
        for (const [type, style] of Object.entries(RELATION_STYLES)) {
            defs.append('marker')
                .attr('id', 'arrow-' + type)
                .attr('viewBox', '0 -5 10 10')
                .attr('refX', 20)
                .attr('refY', 0)
                .attr('markerWidth', 6)
                .attr('markerHeight', 6)
                .attr('orient', 'auto')
                .append('path')
                .attr('d', 'M0,-5L10,0L0,5')
                .attr('fill', style.color);
        }

        // Zoom container
        const g = svg.append('g');

        currentZoom = zoom()
            .scaleExtent([0.1, 8])
            .on('zoom', (event) => {
                g.attr('transform', event.transform);
            });
        svg.call(currentZoom);

        // Draw links
        const link = g.append('g')
            .selectAll('line')
            .data(linkObjects)
            .join('line')
            .attr('stroke', d => (RELATION_STYLES[d.type] || RELATION_STYLES['related']).color)
            .attr('stroke-width', d => d.auto_extracted ? 1 : 1.5)
            .attr('stroke-dasharray', d => (RELATION_STYLES[d.type] || RELATION_STYLES['related']).dash)
            .attr('stroke-opacity', d => d.auto_extracted ? 0.4 : 0.7)
            .attr('marker-end', d => 'url(#arrow-' + d.type + ')');

        // Link labels
        const linkLabel = g.append('g')
            .selectAll('text')
            .data(linkObjects)
            .join('text')
            .text(d => d.type)
            .attr('font-size', 9)
            .attr('fill', 'var(--text-muted)')
            .attr('text-anchor', 'middle')
            .attr('dy', -4)
            .style('pointer-events', 'none')
            .style('opacity', 0.6);

        // Draw nodes
        const node = g.append('g')
            .selectAll('g')
            .data(nodeObjects)
            .join('g')
            .style('cursor', 'pointer')
            .on('click', (event, d) => {
                event.stopPropagation();
                graphSelectedNode.value = d;
            });

        // Node circles — sized by connection count
        node.append('circle')
            .attr('r', d => Math.min(20, 5 + (connectionCount[d.id] || 0) * 2))
            .attr('fill', d => nsColor(d.namespace))
            .attr('stroke', 'var(--bg-surface)')
            .attr('stroke-width', 1.5)
            .attr('opacity', 0.85);

        // Node labels — show just the last part of the slug
        node.append('text')
            .text(d => {
                const parts = d.slug.split('/');
                return parts[parts.length - 1];
            })
            .attr('font-size', 10)
            .attr('fill', 'var(--text-primary)')
            .attr('dx', d => Math.min(20, 5 + (connectionCount[d.id] || 0) * 2) + 4)
            .attr('dy', 3)
            .style('pointer-events', 'none');

        // Namespace badge
        node.append('text')
            .text(d => d.namespace)
            .attr('font-size', 8)
            .attr('fill', d => nsColor(d.namespace))
            .attr('dx', d => Math.min(20, 5 + (connectionCount[d.id] || 0) * 2) + 4)
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

        // Force simulation
        simulation = forceSimulation(nodeObjects)
            .force('link', forceLink(linkObjects).id(d => d.id).distance(120))
            .force('charge', forceManyBody().strength(-300))
            .force('center', forceCenter(width / 2, height / 2))
            .force('collide', forceCollide(30))
            .on('tick', () => {
                link
                    .attr('x1', d => d.source.x)
                    .attr('y1', d => d.source.y)
                    .attr('x2', d => d.target.x)
                    .attr('y2', d => d.target.y);

                linkLabel
                    .attr('x', d => (d.source.x + d.target.x) / 2)
                    .attr('y', d => (d.source.y + d.target.y) / 2);

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
        graphTypeFilter,
        graphShowAuto,
        graphSelectedNode,
        loadGraphData,
        renderGraph,
        destroyGraph,
        nsColor,
    };
}
